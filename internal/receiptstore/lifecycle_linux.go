//go:build linux

package receiptstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

// Reference is a stable owner identity. A zero ExpiresAt means it remains live
// until released. Recovery ownership is created only by receipt acceptance.
type Reference struct {
	Kind, Key string
	ExpiresAt time.Time
}

func referenceValid(r Reference) bool {
	return r.Key != "" && len(r.Key) <= 256 &&
		(r.Kind == "mailbox" || r.Kind == "delivery" || r.Kind == "pending-mailbox")
}

func lockReceipt(ctx context.Context, tx pgx.Tx, org, receipt ID) (string, error) {
	var state string
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			LockReceipt(ctx, dbgen.LockReceiptParams{ID: receipt.String(), Organization: org.String()})
		if sqlcErr != nil {
			return sqlcErr
		}
		state = sqlcResult
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return state, err
}

// reconcileLifetime runs under the receipt guard. Natural expiry uses the last
// reference's deadline, not the delayed sweep time. Released owners are handled
// at their release instant. Once set, eligibility and cleanup_due never move.
func (s *Store) reconcileLifetime(ctx context.Context, tx pgx.Tx, receipt ID) (bool, error) {
	var live bool
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).StoreReconcileLifetimeCheckReference(ctx, receipt.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		live = sqlcResult
		return nil
	}()
	if err != nil || live {
		return live, err
	}
	_, err = dbgen.New(tx).StoreReconcileLifetimeUpdateReceipt(ctx, receipt.String())
	return false, err
}

// AddReference cannot revive an expired/retiring receipt or a released identity.
// Transfer pending ownership by adding its mailbox reference before releasing
// the pending reference. Release alone never starts a fresh recovery period.
func (s *Store) AddReference(ctx context.Context, org, receipt ID, ref Reference) error {
	if !referenceValid(ref) {
		return fmt.Errorf("receiptstore: invalid reference")
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err = s.lockLiveReceipt(ctx, tx, org, receipt); err != nil {
		return err
	}
	if err = addReceiptReference(ctx, tx, receipt, ref); err != nil {
		return err
	}
	return commit(ctx, tx)
}

func (s *Store) ReleaseReference(ctx context.Context, org, receipt ID, ref Reference) error {
	if !referenceValid(ref) {
		return fmt.Errorf("receiptstore: invalid reference")
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if _, err = lockReceipt(ctx, tx, org, receipt); err != nil {
		return err
	}
	// Capture a natural expiry before deleting its historical timestamp.
	if _, err = s.reconcileLifetime(ctx, tx, receipt); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).
		StoreReleaseReferenceInsertReferenceRelease(ctx, dbgen.StoreReleaseReferenceInsertReferenceReleaseParams{
			Receipt:      receipt.String(),
			Kind:         ref.Kind,
			ReferenceKey: ref.Key,
		}); err != nil {
		return err
	}
	// Preserve the release time as an expired reference. This keeps eligibility
	// correct if the other references expired earlier, or a sweep runs much later.
	if _, err = dbgen.New(tx).
		StoreReleaseReferenceUpdateReference(ctx, dbgen.StoreReleaseReferenceUpdateReferenceParams{
			Receipt:      receipt.String(),
			Kind:         ref.Kind,
			ReferenceKey: ref.Key,
		}); err != nil {
		return err
	}
	if _, err = s.reconcileLifetime(ctx, tx, receipt); err != nil {
		return err
	}
	return commit(ctx, tx)
}

// SweepExpiry examines a bounded candidate batch. It never deletes bytes or
// declares handles closed. Repeat it while results are nonzero.
func (s *Store) SweepExpiry(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1024 {
		return 0, fmt.Errorf("receiptstore: invalid sweep limit")
	}
	rows, err := dbgen.New(s.db.pool).StoreSweepExpiry(ctx, int32(limit))
	if err != nil {
		return 0, err
	}
	candidates, err := decodeExpiryCandidates(rows)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, c := range candidates {
		live, e := s.sweepReceipt(ctx, c)
		if e != nil {
			return count, e
		}
		if !live {
			count++
		}
	}
	return count, nil
}

// CleanupStatus deliberately includes inaccessible hot copies and expired but
// unclosed readers. Overdue is evidence of debt, not permission to erase live data.
type CleanupStatus struct {
	State                      string
	EligibleAt, Due, RemovedAt *time.Time
	HotCopies, Packs, Readers  int
	Overdue, DeadlineMissed    bool
}

func (s *Store) CleanupStatus(ctx context.Context, org, receipt ID) (CleanupStatus, error) {
	var result CleanupStatus
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(s.db.pool).
			StoreCleanupStatus(ctx, dbgen.StoreCleanupStatusParams{ID: receipt.String(), Organization: org.String()})
		if sqlcErr != nil {
			return sqlcErr
		}
		result.State = sqlcResult.Lifecycle
		result.EligibleAt = sqlcResult.EligibleAt
		result.Due = sqlcResult.CleanupDue
		result.RemovedAt = sqlcResult.RemovedAt
		result.HotCopies = int(sqlcResult.Count)
		result.Packs = int(sqlcResult.Count_2)
		result.Readers = int(sqlcResult.Count_3)
		result.Overdue = sqlcResult.Column8
		result.DeadlineMissed = sqlcResult.Column9
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return result, err
}

type coldLocation struct {
	Key, ETag string
	Locator   hotstore.Locator
}

// ReadHandle owns the actual reader. Close cancels and waits for the underlying
// file/object read to finish before acknowledging pin release. The maximum
// lifetime applies even if a caller stops consuming without closing the handle.
type ReadHandle struct {
	*io.PipeReader
	cancel   context.CancelFunc
	done     chan struct{}
	closeErr error
	once     sync.Once
}

func (r *ReadHandle) Close() error {
	r.once.Do(func() { r.cancel(); finishPipe(r.PipeReader, nil); <-r.done })
	return r.closeErr
}

func (s *Store) Open(ctx context.Context, org, receipt ID, offset, length int64) (*ReadHandle, error) {
	started := time.Now()
	tx, err := s.db.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	if err = s.lockLiveReceipt(ctx, tx, org, receipt); err != nil {
		return nil, err
	}
	pin, cold, err := s.prepareReadPin(ctx, tx, receipt, offset, length)
	if err != nil {
		return nil, err
	}
	if err = commit(ctx, tx); err != nil {
		return nil, err
	} // Ambiguous insertion stays as debt.
	request := pinnedReadRequest{organization: org, receipt: receipt, pin: pin, offset: offset, length: length}
	return s.openReadHandle(ctx, started, request, cold), nil
}

func (s *Store) lockLiveReceipt(ctx context.Context, tx pgx.Tx, org, receipt ID) error {
	state, err := lockReceipt(ctx, tx, org, receipt)
	if err != nil {
		return err
	}
	live, err := s.reconcileLifetime(ctx, tx, receipt)
	if err != nil {
		return err
	}
	if state != "live" || !live {
		if err = commit(ctx, tx); err != nil {
			return err
		}
		return ErrExpired
	}
	return nil
}

func addReceiptReference(ctx context.Context, tx pgx.Tx, receipt ID, ref Reference) error {
	var released bool
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			StoreAddReferenceCheckReferenceRelease(ctx, dbgen.StoreAddReferenceCheckReferenceReleaseParams{
				Receipt:      receipt.String(),
				Kind:         ref.Kind,
				ReferenceKey: ref.Key,
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		released = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	if released {
		return ErrConflict
	}
	params := dbgen.StoreAddReferenceInsertReferenceParams{
		Receipt:      receipt.String(),
		Kind:         ref.Kind,
		ReferenceKey: ref.Key,
	}
	if err := setReferenceExpiry(ctx, tx, ref, &params); err != nil {
		return err
	}
	tag, err := dbgen.New(tx).StoreAddReferenceInsertReference(ctx, params)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var matches bool
		if err := func() error {
			sqlcResult, sqlcErr := dbgen.New(tx).
				StoreAddReferenceReadSqlc(ctx, dbgen.StoreAddReferenceReadSqlcParams{
					Receipt:      receipt.String(),
					Kind:         ref.Kind,
					ReferenceKey: ref.Key,
					ExpiresAt:    params.ExpiresAt,
				})
			if sqlcErr != nil {
				return sqlcErr
			}
			matches = sqlcResult
			return nil
		}(); err != nil {
			return err
		}
		if !matches {
			return ErrConflict
		}
	}
	return nil
}

func setReferenceExpiry(
	ctx context.Context,
	tx pgx.Tx,
	ref Reference,
	params *dbgen.StoreAddReferenceInsertReferenceParams,
) error {
	if !ref.ExpiresAt.IsZero() {
		var now time.Time
		if err := func() error {
			sqlcResult, sqlcErr := dbgen.New(tx).StoreAddReferenceReadClock(ctx)
			if sqlcErr != nil {
				return sqlcErr
			}
			now = sqlcResult
			return nil
		}(); err != nil {
			return err
		}
		if !ref.ExpiresAt.After(now) {
			return ErrExpired
		}
		params.ExpiresAt = new(ref.ExpiresAt.UTC().Truncate(time.Microsecond))
	}
	return nil
}

type expiryCandidate struct{ id, org ID }

func decodeExpiryCandidates(rows []dbgen.StoreSweepExpiryRow) ([]expiryCandidate, error) {
	var err error
	var candidates []expiryCandidate
	for _, sqlcRow := range rows {
		var a, b string
		a = sqlcRow.ID
		b = sqlcRow.Organization
		id, e := idFrom(a)
		if e != nil {
			err = e
			break
		}
		org, e := idFrom(b)
		if e != nil {
			err = e
			break
		}
		candidates = append(candidates, expiryCandidate{id, org})
	}
	if err != nil {
		return nil, err
	}
	return candidates, nil
}

func (s *Store) sweepReceipt(ctx context.Context, c expiryCandidate) (bool, error) {
	tx, e := s.db.begin(ctx)
	if e != nil {
		return false, e
	}
	_, e = lockReceipt(ctx, tx, c.org, c.id)
	var live bool
	if e == nil {
		live, e = s.reconcileLifetime(ctx, tx, c.id)
	}
	if e == nil {
		e = commit(ctx, tx)
	} else {
		rollback(ctx, tx)
	}
	if e != nil {
		return false, e
	}
	return live, nil
}

func (s *Store) prepareReadPin(
	ctx context.Context,
	tx pgx.Tx,
	receipt ID,
	offset, length int64,
) (ID, *coldLocation, error) {
	stored, err := dbgen.New(tx).StoreOpenReadReceipt(ctx, receipt.String())
	if err != nil {
		return ID{}, nil, err
	}
	size, pack := stored.RawSize, stored.PackID
	if offset < 0 || length < 0 || offset > size || length > size-offset {
		return ID{}, nil, hotstore.ErrInvalidLocator
	}
	var cold *coldLocation
	if pack != nil {
		var err error
		cold, err = readColdLocation(ctx, tx, receipt, *pack)
		if err != nil {
			return ID{}, nil, err
		}
	}
	pin, err := hotstore.NewID()
	if err != nil {
		return ID{}, nil, err
	}
	if _, err = dbgen.New(tx).
		StoreOpenInsertReadPin(ctx, dbgen.StoreOpenInsertReadPinParams{
			ID:      pin.String(),
			Receipt: receipt.String(),
			Pack:    pack,
			Column4: s.db.opts.MaxReadLifetime.Microseconds(),
		}); err != nil {
		return ID{}, nil, err
	}
	return pin, cold, nil
}

func readColdLocation(ctx context.Context, tx pgx.Tx, receipt ID, pack string) (*coldLocation, error) {
	var state string
	location := &coldLocation{}
	var b []byte
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			StoreOpenReadPack(ctx, dbgen.StoreOpenReadPackParams{ID: pack, Receipt: receipt.String()})
		if sqlcErr != nil {
			return sqlcErr
		}
		state = sqlcResult.State
		location.Key = sqlcResult.ObjectKey
		if sqlcResult.Etag == nil {
			return fmt.Errorf("receiptstore: unexpected NULL in StoreOpenReadPack.Etag")
		}
		location.ETag = *sqlcResult.Etag
		b = sqlcResult.Locator
		return nil
	}(); err != nil {
		return nil, err
	}
	if state != "published" {
		return nil, ErrStale
	}
	if err := json.Unmarshal(b, &location.Locator); err != nil {
		return nil, err
	}
	return location, nil
}

func (s *Store) openReadHandle(
	ctx context.Context,
	started time.Time,
	request pinnedReadRequest,
	cold *coldLocation,
) *ReadHandle {
	workCtx, cancel := context.WithDeadline(ctx, started.Add(s.db.opts.MaxReadLifetime))
	reader, writer := io.Pipe()
	handle := &ReadHandle{PipeReader: reader, cancel: cancel, done: make(chan struct{})}
	stop := context.AfterFunc(workCtx, func() { finishPipe(writer, workCtx.Err()) })
	go func() {
		defer close(handle.done)
		defer cancel()
		defer stop()
		request.dst = writer
		readErr := s.readPinned(workCtx, request, cold)
		if errors.Is(readErr, errReadClosureUncertain) {
			handle.closeErr = readErr
		}
		// The storage call has returned: its file lock/body is released. Only this
		// owner can clear its pin. Expiry alone never removes a pin row.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		var pinErr error
		if !errors.Is(readErr, errReadClosureUncertain) {
			_, pinErr = dbgen.New(s.db.pool).StoreOpenDeleteReadPin(cleanupCtx, request.pin.String())
		}
		cleanupCancel()
		finishPipe(writer, errors.Join(readErr, pinErr))
	}()
	return handle
}
