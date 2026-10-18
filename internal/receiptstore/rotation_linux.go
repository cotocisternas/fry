//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RotateHot explicitly seals a bounded current file and starts an empty one.
// Existing targets retain their file IDs, locators, lanes and cleanup debt.
func (s *Store) RotateHot(ctx context.Context, member ID) error {
	return s.dispatchMaintenance(ctx, member, maintenanceKindRotate)
}

func (s *Store) rotateMember(ctx context.Context, m *LocalMember, operation ID, owner *memberOwner, expected ID) error {
	if m.closed {
		return hotstore.ErrClosed
	}
	if m.isFrozen() {
		return ErrUncertain
	}
	if m.opts.MaxRecords == 0 {
		return ErrConflict
	}
	complete, err := m.beginMaintenanceKind(ctx, operation, owner, expected, maintenanceKindRotate)
	if err != nil || complete {
		return err
	}
	err = s.rotateHot(ctx, m, operation, owner)
	if err != nil {
		return m.recoverRotation(ctx, operation, err)
	}
	if err = m.finishMaintenance(ctx, operation, maintenanceComplete); err != nil {
		m.frozen = true
		return err
	}
	return nil
}

type rotationCensus struct {
	records     []hotRecord
	size, count int64
}

func (s *Store) rotationCensus(ctx context.Context, tx pgx.Tx, m *LocalMember) (rotationCensus, error) {
	var c rotationCensus
	records, err := s.snapshotHot(ctx, tx, m)
	if err != nil {
		return c, err
	}
	c, err = countRotationRecords(m, records)
	if err != nil {
		return c, err
	}
	image, err := m.bytes.Snapshot()
	if err != nil {
		return c, err
	}
	size := image.Size()
	if err = image.Close(); err != nil {
		return c, rotationClosureError(m, err)
	}
	if c.count == 0 || c.count > m.opts.MaxRecords || c.size != size {
		return c, ErrConflict
	}
	return c, nil
}

func sameRotationCensus(a, b rotationCensus) bool {
	if a.size != b.size || a.count != b.count || len(a.records) != len(b.records) {
		return false
	}
	for i, r := range a.records {
		if !sameHotRecord(r, b.records[i]) {
			return false
		}
	}
	return true
}

// During reservation extra=C; after successor registration its charge is already
// in the sum and extra=0. The source's exact length is the additional margin
// needed to leave one full rewrite file free in the proposed published state.
func rotationCapacity(ctx context.Context, tx pgx.Tx, m *LocalMember, source, extra int64) error {
	profile, err := dbgen.New(tx).
		RotationCapacityReadMemberProfile(ctx, dbgen.RotationCapacityReadMemberProfileParams{
			Member: m.id.String(),
			FileID: m.bytes.FileID().String(),
		})
	if err != nil {
		return err
	}
	if !validRotationCapacityBinding(profile, source, extra) {
		return ErrConflict
	}
	charged, err := dbgen.New(tx).RotationCapacityCheckMemberFile(ctx, m.id.String())
	if err != nil {
		return err
	}
	available := profile.MaxBytes - profile.StagingBytes
	if charged > available || extra > available-charged || source > available-charged-extra {
		return hotstore.ErrCapacity
	}
	return nil
}

func validRotationCapacityBinding(profile dbgen.RotationCapacityReadMemberProfileRow, source, extra int64) bool {
	return profile.Enabled && profile.ChargedBytes == profile.MaxFileBytes && source >= memberFileHeaderBytes &&
		source <= profile.MaxFileBytes &&
		(extra == 0 || extra == profile.MaxFileBytes)
}

func (s *Store) rotateHot(ctx context.Context, m *LocalMember, operation ID, owner *memberOwner) (resultErr error) {
	r := hotRotation{store: s, member: m, operation: operation, owner: owner}
	if err := m.reconcileRewrites(ctx); err != nil {
		return err
	}
	if err := r.reserve(ctx); err != nil {
		return err
	}
	if err := r.openSuccessor(); err != nil {
		return err
	}
	defer r.closeUnretained(&resultErr)
	if err := r.registerSuccessor(ctx); err != nil {
		return err
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err = r.checkPublication(ctx, tx); err != nil {
		return err
	}
	if err = r.publishCatalog(ctx, tx); err != nil {
		return err
	}
	return r.activate(ctx)
}

type hotRotation struct {
	store          *Store
	member         *LocalMember
	operation, job ID
	owner          *memberOwner
	directory      string
	census         rotationCensus
	next           *hotstore.Store
	retained       bool
}

func (r *hotRotation) reserve(ctx context.Context) error {
	tx, err := r.store.db.begin(ctx)
	if err != nil {
		return err
	}
	if err = r.reserveTx(ctx, tx); err != nil {
		rollback(ctx, tx)
		return err
	}
	return commit(ctx, tx)
}

func (r *hotRotation) reserveTx(ctx context.Context, tx pgx.Tx) error {
	m := r.member
	var err error
	r.census, err = r.store.rotationCensus(ctx, tx, m)
	if err != nil {
		return err
	}
	if err = rotationCapacity(ctx, tx, m, r.census.size, m.opts.MaxFileBytes); err != nil {
		return err
	}
	r.job, err = hotstore.NewID()
	if err != nil {
		return err
	}
	r.directory = "generation-" + r.job.String()
	_, err = dbgen.New(tx).
		StoreRotateHotInsertHotRewrite(ctx, dbgen.StoreRotateHotInsertHotRewriteParams{
			ID:             r.job.String(),
			Member:         m.id.String(),
			OldIncarnation: m.bytes.FileID().String(),
			OldDirectory:   m.directory,
			NewDirectory:   r.directory,
			ReservedBytes:  m.opts.MaxFileBytes,
		})
	return err
}

func (r *hotRotation) openSuccessor() error {
	path := filepath.Join(r.member.root, r.directory)
	if err := os.Mkdir(path, 0o700); err != nil {
		return err
	}
	if err := syncMemberRoot(r.member.root); err != nil {
		return err
	}
	var err error
	r.next, err = hotstore.Open(path, r.member.opts)
	return err
}

func (r *hotRotation) closeUnretained(resultErr *error) {
	if !r.retained {
		if closeErr := r.next.Close(); closeErr != nil {
			*resultErr = errors.Join(*resultErr, rotationClosureError(r.member, closeErr))
		}
	}
}

func (m *LocalMember) checkRewriteFault(phase string) error {
	if m.rewriteFault != nil {
		return m.rewriteFault(phase)
	}
	return nil
}

func (r *hotRotation) registerSuccessor(ctx context.Context) error {
	if err := r.member.checkRewriteFault("before_register"); err != nil {
		return err
	}
	if err := r.member.registerSuccessor(ctx, r.job, r.directory, r.next.FileID()); err != nil {
		return err
	}
	return r.member.checkRewriteFault("after_register")
}

func (r *hotRotation) checkPublication(ctx context.Context, tx pgx.Tx) error {
	m := r.member
	current, err := r.store.rotationCensus(ctx, tx, m)
	if err != nil {
		return err
	}
	if !sameRotationCensus(r.census, current) {
		return ErrStale
	}
	binding, err := dbgen.New(tx).StoreRotateHotReadMember(ctx, m.id.String())
	if err != nil {
		return err
	}
	if binding.Incarnation != m.bytes.FileID().String() || binding.MaintenanceJob == nil ||
		*binding.MaintenanceJob != r.operation.String() {
		return ErrStale
	}
	if r.owner != nil {
		if err = r.owner.guard(ctx, tx); err != nil {
			return err
		}
	}
	if err = rotationCapacity(ctx, tx, m, current.size, 0); err != nil {
		return err
	}
	r.census = current
	return nil
}

func (r *hotRotation) publishCatalog(ctx context.Context, tx pgx.Tx) error {
	m, current := r.member, r.census
	tag, err := dbgen.New(tx).
		StoreRotateHotUpdateMemberFile(ctx, dbgen.StoreRotateHotUpdateMemberFileParams{
			Member:        m.id.String(),
			FileID:        m.bytes.FileID().String(),
			RewriteJob:    sqlcPointer(r.job.String()),
			ChargedBytes:  current.size,
			SealedRecords: sqlcPointer(current.count),
			Directory:     m.directory,
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	tag, err = dbgen.New(tx).
		StoreRotateHotUpdateMemberFileCurrent(ctx, dbgen.StoreRotateHotUpdateMemberFileCurrentParams{
			Member:     m.id.String(),
			FileID:     r.next.FileID().String(),
			RewriteJob: sqlcPointer(r.job.String()),
			Directory:  r.directory,
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err = dbgen.New(tx).
		StoreRotateHotUpdateMember(ctx, dbgen.StoreRotateHotUpdateMemberParams{
			ID:          m.id.String(),
			Incarnation: r.next.FileID().String(),
			Directory:   r.directory,
		}); err != nil {
		return err
	}
	tag, err = dbgen.New(tx).
		StoreRotateHotUpdateHotRewrite(ctx, dbgen.StoreRotateHotUpdateHotRewriteParams{
			ID:             r.job.String(),
			Member:         m.id.String(),
			NewIncarnation: sqlcPointer(r.next.FileID().String()),
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if err = commit(ctx, tx); err != nil {
		return r.fenceSource(err)
	}
	return nil
}

func (r *hotRotation) fenceSource(err error) error {
	r.member.frozen = true
	if closeErr := r.member.bytes.Close(); closeErr != nil {
		err = errors.Join(err, rotationClosureError(r.member, closeErr))
	}
	return err
}

func (r *hotRotation) activate(ctx context.Context) error {
	m := r.member
	if err := m.checkRewriteFault("after_publish"); err != nil {
		return r.fenceSource(err)
	}
	old := m.bytes
	m.bytes, m.directory, r.retained = r.next, r.directory, true
	if err := old.Close(); err != nil {
		return rotationClosureError(m, err)
	}
	if err := m.checkRewriteFault("after_source_close"); err != nil {
		m.frozen = true
		return err
	}
	if err := m.reconcileRewrites(ctx); err != nil {
		m.frozen = true
		return err
	}
	return nil
}

// Sealed bytes are checked read-only at startup; payloads are verified on reads.
// Historical rotation successors may themselves now be sealed or removed.
func preflightSealedFiles(ctx context.Context, pool *pgxpool.Pool, member ID, root string) error {
	rows, err := pool.Query(ctx, dbgen.PreflightSealedFiles, member.String())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var file sealedFileEvidence
		if err = rows.Scan(
			&file.file,
			&file.directory,
			&file.size,
			&file.records,
			&file.maxRecords,
			&file.maxFile,
			&file.enabled,
			&file.valid,
		); err != nil {
			return err
		}
		if err = file.verify(root); err != nil {
			return err
		}
	}
	return rows.Err()
}

// An unproven descriptor close retains root ownership as well as freezing I/O.
func rotationClosureError(m *LocalMember, err error) error {
	m.frozen = true
	m.readClosureUncertain.Store(true)
	return errors.Join(ErrUncertain, err)
}

// Callers hold the exclusive member gate. A later idempotent Close cannot prove
// a descriptor closed when an earlier attempt returned an indeterminate result.
func closeMemberStore(m *LocalMember, store *hotstore.Store) error {
	var err error
	if m.storeClose != nil {
		err = m.storeClose(store)
	} else {
		err = store.Close()
	}
	if err != nil {
		return rotationClosureError(m, err)
	}
	return nil
}

func (m *LocalMember) recoverRotation(ctx context.Context, operation ID, err error) error {
	if errors.Is(err, hotstore.ErrUncertain) {
		m.frozen = true
	}
	if m.isFrozen() {
		return err
	}
	if recoveryErr := m.reconcileRewrites(ctx); recoveryErr != nil {
		m.frozen = true
		return errors.Join(err, recoveryErr)
	}
	if finishErr := m.failMaintenance(ctx, operation); finishErr != nil {
		m.frozen = true
		return errors.Join(err, finishErr)
	}
	return err
}

func countRotationRecords(m *LocalMember, records []hotRecord) (rotationCensus, error) {
	var c rotationCensus
	c.records = records
	c.size = memberFileHeaderBytes
	for _, r := range records {
		if r.locator.File == (ID{}) {
			continue
		}
		if r.locator.File != m.bytes.FileID() {
			return c, ErrConflict
		}
		encoded, e := hotstore.EncodedRecordBytes(r.locator.Size)
		if e != nil {
			return c, e
		}
		if encoded > m.opts.MaxFileBytes-c.size {
			return c, ErrConflict
		}
		c.size += encoded
		c.count++
	}
	return c, nil
}

func sameHotRecord(a, b hotRecord) bool {
	if a.attempt != b.attempt || a.receipt != b.receipt || a.state != b.state || a.locator != b.locator {
		return false
	}
	if (a.lane == nil) != (b.lane == nil) {
		return false
	}
	return a.lane == nil || *a.lane == *b.lane
}

type sealedFileEvidence struct {
	file, directory                    string
	size, records, maxRecords, maxFile int64
	enabled, valid                     bool
}

func (file sealedFileEvidence) validCatalog() bool {
	return file.enabled && file.valid && validMemberDirectory(file.directory) && file.size >= 64 &&
		file.size <= file.maxFile &&
		file.records >= 1 &&
		file.records <= file.maxRecords
}

func (file sealedFileEvidence) verify(root string) error {
	if !file.validCatalog() {
		return ErrConflict
	}
	actual, err := hotstore.InspectFileIdentity(filepath.Join(root, file.directory))
	if err != nil {
		return err
	}
	if actual.String() != file.file {
		return ErrConflict
	}
	info, err := storageStat(root, filepath.Join(file.directory, "data.fry"))
	if err != nil {
		return err
	}
	if info.Size() != file.size {
		return ErrConflict
	}
	return nil
}
