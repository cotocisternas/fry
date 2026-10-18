//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type operation struct {
	cancel context.CancelFunc
	source *ownedStream
	done   chan struct{}
}
type memberStripe struct {
	sync.Mutex
	active map[ID]*operation
}

// LocalMember is an in-process member with a distinct local file incarnation.
// Domain is a trusted placement label, not evidence of physical independence.
// The future remote adapter must authenticate members and fence disk ownership.
type LocalMember struct {
	gate                 sync.RWMutex
	root, directory      string
	owner                *os.File
	closed               bool
	frozen               bool
	readClosureUncertain atomic.Bool
	readPermits          chan struct{}
	imageClose           func(*hotstore.Image) error // optional per-instance closure-proof fault seam
	storeClose           func(*hotstore.Store) error // optional per-instance closure-proof fault seam
	transportOwned       bool
	opts                 hotstore.Options
	id                   ID
	domain               string
	bytes                *hotstore.Store
	pool                 *pgxpool.Pool
	stripes              [32]memberStripe
	rewriteFault         func(string) error
	sealedCursor         string // scheduling only, protected by gate
}

func (m *LocalMember) isFrozen() bool { return m.frozen || m.readClosureUncertain.Load() }

// OpenLocalMember bounds each metadata stage independently. The caller owns
// the overall initialization deadline; the synchronous byte-store scan cannot
// be interrupted, so cancellation is checked before and after it. Successful
// initialization owns the store and its registry until Close. Opening
// another member on the same directory fails catalog preflight or root locking;
// callers cannot accidentally give one file two independent cancellation registries.
func OpenLocalMember(
	ctx context.Context,
	pool *pgxpool.Pool,
	id ID,
	domain, dir string,
	opts hotstore.Options,
) (*LocalMember, error) {
	return openLocalMember(
		ctx,
		pool,
		id,
		domain,
		dir,
		opts,
		memberInitialization{metadataTimeout: 30 * time.Second, openStore: hotstore.Open},
	)
}

// memberInitialization varies only the filesystem-open boundary and metadata
// stage budget for a single initialization; it never changes process lifetime.
type memberInitialization struct {
	metadataTimeout time.Duration
	openStore       func(string, hotstore.Options) (*hotstore.Store, error)
}

func openLocalMember(
	parent context.Context,
	pool *pgxpool.Pool,
	id ID,
	domain, dir string,
	opts hotstore.Options,
	initialization memberInitialization,
) (*LocalMember, error) {
	if pool == nil || id == (ID{}) || domain == "" {
		return nil, fmt.Errorf("receiptstore: invalid member")
	}
	ctx, cancel := context.WithTimeout(parent, initialization.metadataTimeout)
	defer cancel()
	if err := preflightMemberFiles(ctx, pool, id, dir, opts); err != nil {
		return nil, err
	}
	owner, err := lockMemberRoot(ctx, pool, id, dir, opts)
	if err != nil {
		return nil, err
	}
	binding, err := localMemberBinding(ctx, pool, id, domain)
	if err != nil {
		return nil, errors.Join(err, owner.Close())
	}
	directory := binding.directory
	// A valid-prefix scan grows with retained bytes. Do not charge that scan
	// against the preceding or following metadata-stage budget.
	cancel()
	if err := parent.Err(); err != nil {
		return nil, errors.Join(err, owner.Close())
	}
	store, err := initialization.openStore(filepath.Join(dir, directory), opts)
	if err != nil {
		return nil, errors.Join(err, owner.Close())
	}
	ctx, cancel = context.WithTimeout(parent, initialization.metadataTimeout)
	defer cancel()
	m := &LocalMember{
		id:          id,
		domain:      domain,
		bytes:       store,
		pool:        pool,
		root:        dir,
		directory:   directory,
		owner:       owner,
		opts:        opts,
		readPermits: make(chan struct{}, 32),
	}
	err = m.finishInitialization(ctx, binding)
	if err != nil {
		return nil, errors.Join(err, m.Close())
	}
	return m, nil
}

// Close waits for owned streams and reads before releasing the member root lock.
func (m *LocalMember) Close() error {
	m.gate.Lock()
	defer m.gate.Unlock()
	if m.readClosureUncertain.Load() {
		return ErrUncertain
	}
	if m.closed {
		return nil
	}
	if err := closeMemberStore(m, m.bytes); err != nil {
		return err
	}
	if err := m.owner.Close(); err != nil {
		return rotationClosureError(m, err)
	}
	m.closed = true
	return nil
}

func (m *LocalMember) stripe(id ID) *memberStripe { return &m.stripes[int(id[0])%len(m.stripes)] }
func (m *LocalMember) write(ctx context.Context, a *authority, c claim, source io.ReadCloser) error {
	return m.writeOwned(ctx, a, c, source, nil)
}

func (m *LocalMember) writeOwned(
	ctx context.Context,
	a *authority,
	c claim,
	source io.ReadCloser,
	owner *memberOwner,
) (resultErr error) {
	ownedSource := &ownedStream{ReadCloser: source}
	source = ownedSource
	defer func() { resultErr = errors.Join(resultErr, ownedSource.Close()) }()
	m.gate.RLock()
	defer m.gate.RUnlock()
	if m.closed || m.isFrozen() || (m.transportOwned && owner == nil) {
		ownedSource.cancelRead()
		return hotstore.ErrClosed
	}
	stripe := m.stripe(c.Attempt)
	stripe.Lock()
	if stripe.active == nil {
		stripe.active = make(map[ID]*operation)
	}
	if stripe.active[c.Attempt] != nil {
		stripe.Unlock()
		ownedSource.cancelRead()
		return ErrBusy
	}
	if _, err := a.validateTarget(ctx, c, m, owner); err != nil {
		stripe.Unlock()
		ownedSource.cancelRead()
		return err
	}
	workCtx, cancel := context.WithCancel(ctx)
	op := &operation{cancel: cancel, source: ownedSource, done: make(chan struct{})}
	stripe.active[c.Attempt] = op
	stripe.Unlock()
	stop := context.AfterFunc(workCtx, func() { ownedSource.cancelRead() })
	defer func() {
		cancel()
		stop()
		ownedSource.cancelRead()
		stripe.Lock()
		delete(stripe.active, c.Attempt)
		close(op.done)
		stripe.Unlock()
	}()
	loc, err := m.bytes.Append(workCtx, hotstore.Key{Receipt: c.Receipt, Attempt: c.Attempt}, source)
	if err != nil {
		return err
	}
	return a.recordCopy(workCtx, c, m, loc, owner)
}

func (m *LocalMember) cancelAttempt(ctx context.Context, a *authority, attempt ID) error {
	return m.cancelOwned(ctx, a, attempt, nil)
}

func (m *LocalMember) cancelOwned(ctx context.Context, a *authority, attempt ID, owner *memberOwner) error {
	m.gate.RLock()
	defer m.gate.RUnlock()
	stripe := m.stripe(attempt)
	stripe.Lock()
	// Check terminal authority under the same lock used to start an operation.
	var state string
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(a.pool).LocalMemberCancelOwned(ctx, attempt.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		state = sqlcResult
		return nil
	}()
	if err != nil {
		stripe.Unlock()
		return err
	}
	if state != "aborted" {
		stripe.Unlock()
		return ErrStale
	}
	op := stripe.active[attempt]
	if op != nil {
		op.cancel()
		op.source.cancelRead()
	}
	stripe.Unlock()
	if op != nil {
		select {
		case <-op.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// An operation queued after the lock is released rechecks terminal DB state.
	return a.confirmCancellation(ctx, attempt, m, owner)
}

func (s *Store) Abort(ctx context.Context, organization, attempt ID) error {
	if err := s.db.abort(ctx, organization, attempt); err != nil {
		return err
	}
	targets, errs, err := s.abortTargets(ctx, attempt)
	if err != nil {
		return err
	}
	for _, m := range targets {
		if err := m.cancelAttempt(ctx, s.db, attempt); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func lockMemberRoot(
	ctx context.Context,
	pool *pgxpool.Pool,
	id ID,
	dir string,
	opts hotstore.Options,
) (*os.File, error) {
	owner, err := openStorageFile(dir, "member.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(owner.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("%w: %w", hotstore.ErrBusy, err), owner.Close())
	}
	if err = preflightMemberFiles(ctx, pool, id, dir, opts); err != nil {
		return nil, errors.Join(err, owner.Close())
	}
	return owner, nil
}

type localMemberRegistration struct {
	incarnation, directory string
	fresh                  bool
}

func localMemberBinding(
	ctx context.Context,
	pool *pgxpool.Pool,
	id ID,
	domain string,
) (localMemberRegistration, error) {
	row, err := dbgen.New(pool).OpenLocalMemberReadMember(ctx, id.String())
	binding := localMemberRegistration{
		incarnation: row.Incarnation,
		directory:   row.Directory,
		fresh:       errors.Is(err, pgx.ErrNoRows),
	}
	if err != nil && !binding.fresh {
		return binding, err
	}
	if binding.fresh {
		binding.directory = "."
	} else if row.FailureDomain != domain {
		return binding, ErrConflict
	}
	// Filesystem recovery cannot authorize takeover of an unclosed generation.
	unclosed, err := dbgen.New(pool).OpenLocalMemberCheckMemberOwner(ctx, id.String())
	if err != nil {
		return binding, err
	}
	if unclosed {
		return binding, ErrBusy
	}
	if !validMemberDirectory(binding.directory) {
		return binding, ErrConflict
	}
	return binding, nil
}

func (m *LocalMember) finishInitialization(ctx context.Context, binding localMemberRegistration) error {
	var err error
	if binding.fresh {
		err = m.registerInitialFiles(ctx)
	} else if binding.incarnation != m.bytes.FileID().String() {
		err = ErrConflict
	}
	if err != nil {
		return err
	}
	if err = m.reconcileRewrites(ctx); err != nil {
		return err
	}
	return m.recoverMaintenance(ctx)
}

func (m *LocalMember) registerInitialFiles(ctx context.Context) error {
	tx, err := (&authority{pool: m.pool}).begin(ctx)
	if err != nil {
		return err
	}
	_, err = dbgen.New(tx).
		OpenLocalMemberInsertMember(ctx, dbgen.OpenLocalMemberInsertMemberParams{
			ID:            m.id.String(),
			Incarnation:   m.bytes.FileID().String(),
			FailureDomain: m.domain,
			Directory:     m.directory,
		})
	if err == nil {
		_, err = dbgen.New(tx).
			OpenLocalMemberInsertMemberFile(ctx, dbgen.OpenLocalMemberInsertMemberFileParams{
				Member:       m.id.String(),
				FileID:       m.bytes.FileID().String(),
				Directory:    m.directory,
				ChargedBytes: m.opts.MaxFileBytes,
			})
	}
	if err == nil {
		err = insertMemberProfile(ctx, tx, m.id, m.opts)
	}
	if err == nil {
		err = insertCapacityLanes(ctx, tx, m.id, m.bytes.FileID(), m.opts)
	}
	if err == nil {
		err = commit(ctx, tx)
	} else {
		rollback(ctx, tx)
	}
	return err
}

func (s *Store) abortTargets(ctx context.Context, attempt ID) ([]Member, []error, error) {
	var errs []error
	rows, err := s.db.pool.Query(ctx, dbgen.StoreAbort, attempt.String())
	if err != nil {
		return nil, nil, err
	}
	var targets []Member
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			break
		}
		id, e := idFrom(raw)
		if e != nil {
			err = e
			break
		}
		m := s.memberByID[id]
		if m == nil {
			errs = append(errs, ErrNotFound)
		} else {
			targets = append(targets, m)
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	if rowErr != nil {
		return nil, nil, rowErr
	}
	return targets, errs, nil
}
