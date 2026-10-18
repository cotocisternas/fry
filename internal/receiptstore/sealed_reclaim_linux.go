//go:build linux

package receiptstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReclaimSealed considers one internally selected sealed file. It never changes
// the current append file. Selection progresses over a finite stable file set;
// no sealed files, or no disposable records in the selected file, is a no-op.
func (s *Store) ReclaimSealed(ctx context.Context, member ID) error {
	return s.dispatchMaintenance(ctx, member, maintenanceKindSealedReclaim)
}

func (s *Store) reclaimSealedMember(
	ctx context.Context,
	m *LocalMember,
	operation ID,
	owner *memberOwner,
	expected ID,
) error {
	if m.closed {
		return hotstore.ErrClosed
	}
	if m.isFrozen() {
		return ErrUncertain
	}
	complete, err := m.beginMaintenanceKind(ctx, operation, owner, expected, maintenanceKindSealedReclaim)
	if err != nil || complete {
		return err
	}
	err = s.reclaimSealed(ctx, m, operation, owner)
	if err != nil {
		return m.failSealedReclaim(ctx, operation, err)
	}
	if err = m.finishMaintenance(ctx, operation, maintenanceComplete); err != nil {
		m.frozen = true
	}
	return err
}

func (m *LocalMember) failSealedReclaim(ctx context.Context, operation ID, err error) error {
	if errors.Is(err, hotstore.ErrUncertain) || errors.Is(err, ErrUncertain) {
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

type sealedSource struct {
	file        ID
	directory   string
	size, count int64
}

//nolint:nilnil // A nil candidate with no error means the bounded scan found no reclaimable file.
func (m *LocalMember) selectSealed(ctx context.Context, operation ID) (*sealedSource, error) {
	tx, err := (&authority{pool: m.pool}).begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	selected, err := dbgen.New(tx).
		LocalMemberSelectSealedReadMemberMaintenance(ctx, dbgen.LocalMemberSelectSealedReadMemberMaintenanceParams{
			ID:     operation.String(),
			Member: m.id.String(),
		})
	if err != nil {
		return nil, err
	}
	source, err := m.readSealedSelection(ctx, tx, operation, selected)
	if selected == nil && errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = m.validateSealedSelection(source); err != nil {
		return nil, err
	}
	if err = commit(ctx, tx); err != nil {
		m.frozen = true
		return nil, err
	}
	return &source, nil
}

func (m *LocalMember) readSealedSelection(
	ctx context.Context,
	tx pgx.Tx,
	operation ID,
	selected *string,
) (sealedSource, error) {
	var file, directory string
	var size int64
	var count *int64
	if selected == nil {
		row, err := dbgen.New(tx).
			LocalMemberSelectSealedReadMemberFile(ctx, dbgen.LocalMemberSelectSealedReadMemberFileParams{
				Member: m.id.String(),
				FileID: m.sealedCursor,
			})
		if err != nil {
			return sealedSource{}, err
		}
		file, directory, size, count = row.FileID, row.Directory, row.ChargedBytes, row.SealedRecords
		if count == nil {
			return sealedSource{}, fmt.Errorf(
				"receiptstore: unexpected NULL in LocalMemberSelectSealedReadMemberFile.SealedRecords",
			)
		}
		m.sealedCursor = file
		tag, err := dbgen.New(tx).
			LocalMemberSelectSealedUpdateMemberMaintenance(
				ctx,
				dbgen.LocalMemberSelectSealedUpdateMemberMaintenanceParams{
					ID:           operation.String(),
					Member:       m.id.String(),
					SelectedFile: new(file),
				},
			)
		if err != nil {
			return sealedSource{}, err
		}
		if tag.RowsAffected() != 1 {
			return sealedSource{}, ErrConflict
		}
	} else {
		row, err := dbgen.New(tx).
			LocalMemberSelectSealedReadMemberFileSealed(ctx, dbgen.LocalMemberSelectSealedReadMemberFileSealedParams{
				Member: m.id.String(),
				FileID: *selected,
			})
		if err != nil {
			return sealedSource{}, err
		}
		file, directory, size, count = row.FileID, row.Directory, row.ChargedBytes, row.SealedRecords
		if count == nil {
			return sealedSource{}, fmt.Errorf(
				"receiptstore: unexpected NULL in LocalMemberSelectSealedReadMemberFileSealed.SealedRecords",
			)
		}
	}
	id, err := idFrom(file)
	return sealedSource{id, directory, size, *count}, err
}

func (m *LocalMember) validateSealedSelection(source sealedSource) error {
	if source.file == m.bytes.FileID() || !validMemberDirectory(source.directory) || source.size < 64 ||
		source.size > m.opts.MaxFileBytes ||
		source.count < 1 ||
		source.count > m.opts.MaxRecords {
		return ErrConflict
	}
	return nil
}

func closeSealedImage(m *LocalMember, image *hotstore.Image) error {
	var err error
	if m.imageClose != nil {
		err = m.imageClose(image)
	} else {
		err = image.Close()
	}
	if err != nil {
		return rotationClosureError(m, err)
	}
	return nil
}

func (s *Store) reclaimSealed(ctx context.Context, m *LocalMember, operation ID, owner *memberOwner) (resultErr error) {
	if err := m.reconcileRewrites(ctx); err != nil {
		return err
	}
	source, err := m.selectSealed(ctx, operation)
	if err != nil || source == nil {
		return err
	}
	return s.reclaimSealedFile(ctx, m, operation, owner, *source)
}

// sealedReclaim keeps the census and replacement evidence together across the
// reservation and publication transactions. The source image closes before any
// catalog publication, including delete-only jobs.
type sealedReclaim struct {
	store          *Store
	member         *LocalMember
	operation, job ID
	owner          *memberOwner
	source         sealedSource
	image          *hotstore.Image
	physical       []hotstore.Locator
	records        []hotRecord
	kept           int64
	directory      string
	nextID         ID
	nextSize       int64
	locators       map[ID]hotstore.Locator
}

func (s *Store) reclaimSealedFile(
	ctx context.Context,
	m *LocalMember,
	operation ID,
	owner *memberOwner,
	source sealedSource,
) (resultErr error) {
	image, err := m.openSealedReclaimImage(source)
	if err != nil {
		return err
	}
	imageOpen := true
	defer func() {
		if imageOpen {
			resultErr = errors.Join(resultErr, closeSealedImage(m, image))
		}
	}()
	r := sealedReclaim{
		store:     s,
		member:    m,
		operation: operation,
		owner:     owner,
		source:    source,
		image:     image,
		nextSize:  64,
		locators:  map[ID]hotstore.Locator{},
	}
	needed, err := r.reserve(ctx)
	if err != nil || !needed {
		return err
	}
	if err = r.buildSuccessor(ctx); err != nil {
		return err
	}
	err = closeSealedImage(m, image)
	imageOpen = false
	if err != nil {
		return err
	}
	if err = m.checkRewriteFault("after_copy"); err != nil {
		return err
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err = r.publish(ctx, tx); err != nil {
		return err
	}
	return r.finish(ctx)
}

func (m *LocalMember) openSealedReclaimImage(source sealedSource) (*hotstore.Image, error) {
	if source.file == m.bytes.FileID() {
		return nil, ErrConflict
	}
	// The exclusive member gate has drained actual readers. Queued remote
	// readers acquire read permits before that gate, so do not wait here.
	image, err := hotstore.OpenFileImage(filepath.Join(m.root, source.directory), source.file, source.size)
	if errors.Is(err, hotstore.ErrUncertain) {
		return nil, rotationClosureError(m, err)
	}
	return image, err
}

func (r *sealedReclaim) reserve(ctx context.Context) (bool, error) {
	m := r.member
	var err error
	r.physical, err = r.image.Inventory(
		ctx,
		hotstore.InventoryLimits{
			MaxRecords:      m.opts.MaxRecords,
			MaxFileBytes:    m.opts.MaxFileBytes,
			MaxMessageBytes: m.opts.MaxMessageBytes,
		},
	)
	if err != nil {
		return false, err
	}
	if int64(len(r.physical)) != r.source.count {
		return false, ErrConflict
	}
	tx, err := r.store.db.begin(ctx)
	if err != nil {
		return false, err
	}
	needed, err := r.reserveTx(ctx, tx)
	if err != nil || !needed {
		rollback(ctx, tx)
		return false, err
	}
	if err = commit(ctx, tx); err != nil {
		m.frozen = true
	}
	return true, err
}

func (r *sealedReclaim) reserveTx(ctx context.Context, tx pgx.Tx) (bool, error) {
	var err error
	r.records, err = r.store.snapshotHotFile(ctx, tx, r.member, r.source.file, r.physical)
	if err != nil {
		return false, err
	}
	if !hasDroppedHotRecords(r.records) {
		return false, nil
	}
	for _, record := range r.records {
		if record.keep {
			r.kept++
		}
	}
	r.job, err = hotstore.NewID()
	if err != nil {
		return false, err
	}
	var charge int64
	if r.kept > 0 {
		charge, err = reserveRewriteCapacity(ctx, tx, r.member.id)
		if err != nil {
			return false, err
		}
	}
	r.directory = "generation-" + r.job.String()
	_, err = dbgen.New(tx).
		StoreReclaimSealedFileInsertHotRewrite(ctx, dbgen.StoreReclaimSealedFileInsertHotRewriteParams{
			ID:             r.job.String(),
			Member:         r.member.id.String(),
			OldIncarnation: r.source.file.String(),
			OldDirectory:   r.source.directory,
			NewDirectory:   r.directory,
			ReservedBytes:  charge,
			KeptRecords:    new(r.kept),
		})
	return true, err
}

func (r *sealedReclaim) buildSuccessor(ctx context.Context) (resultErr error) {
	if r.kept == 0 {
		return nil
	}
	m := r.member
	path := filepath.Join(m.root, r.directory)
	if err := os.Mkdir(path, 0o700); err != nil {
		return err
	}
	if err := syncMemberRoot(m.root); err != nil {
		return err
	}
	next, err := hotstore.Open(path, m.opts)
	if err != nil {
		return err
	}
	nextOpen := true
	defer func() {
		if nextOpen {
			if closeErr := next.Close(); closeErr != nil {
				resultErr = errors.Join(resultErr, rotationClosureError(m, closeErr))
			}
		}
	}()
	r.nextID = next.FileID()
	if err = r.registerSuccessor(ctx); err != nil {
		return err
	}
	if err = r.copyRecords(ctx, next); err != nil {
		return err
	}
	err = next.Close()
	nextOpen = false
	if err != nil {
		return rotationClosureError(m, err)
	}
	return nil
}

func (r *sealedReclaim) registerSuccessor(ctx context.Context) error {
	m := r.member
	if err := m.checkRewriteFault("before_register"); err != nil {
		return err
	}
	if err := m.registerSuccessor(ctx, r.job, r.directory, r.nextID); err != nil {
		return err
	}
	return m.checkRewriteFault("after_register")
}

func (r *sealedReclaim) copyRecords(ctx context.Context, next *hotstore.Store) error {
	ordered := append([]hotRecord(nil), r.records...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].locator.Offset < ordered[j].locator.Offset })
	for _, record := range ordered {
		if !record.keep {
			continue
		}
		loc, err := copyHotRecord(ctx, next, record.locator, r.image.ReadRecord)
		if err != nil {
			return err
		}
		encoded, err := hotstore.EncodedRecordBytes(loc.Size)
		if err != nil {
			return err
		}
		r.nextSize += encoded
		r.locators[record.receipt] = loc
	}
	return nil
}

func (r *sealedReclaim) publish(ctx context.Context, tx pgx.Tx) error {
	if err := r.checkPublication(ctx, tx); err != nil {
		return err
	}
	if r.kept > 0 {
		if err := r.publishSuccessor(ctx, tx); err != nil {
			return err
		}
	}
	return r.publishRetirement(ctx, tx)
}

func (r *sealedReclaim) checkPublication(ctx context.Context, tx pgx.Tx) error {
	m := r.member
	current, err := r.store.snapshotHotFile(ctx, tx, m, r.source.file, r.physical)
	if err != nil {
		return err
	}
	if !sameSealedCensus(r.records, current) {
		return ErrStale
	}
	binding, err := dbgen.New(tx).StoreReclaimSealedFileReadMember(ctx, m.id.String())
	if err != nil {
		return err
	}
	if binding.Incarnation != m.bytes.FileID().String() || binding.MaintenanceJob == nil ||
		*binding.MaintenanceJob != r.operation.String() {
		return ErrStale
	}
	if r.owner != nil {
		return r.owner.guard(ctx, tx)
	}
	return nil
}

func (r *sealedReclaim) publishSuccessor(ctx context.Context, tx pgx.Tx) error {
	m := r.member
	if err := setSuccessorCapacity(ctx, tx, m.id, r.nextID, r.job, m.opts, r.records, r.locators); err != nil {
		return err
	}
	for _, record := range r.records {
		if !record.keep {
			continue
		}
		if err := r.publishLocator(ctx, tx, record); err != nil {
			return err
		}
	}
	tag, err := dbgen.New(tx).
		StoreReclaimSealedFileUpdateMemberFile(ctx, dbgen.StoreReclaimSealedFileUpdateMemberFileParams{
			Member:        m.id.String(),
			FileID:        r.nextID.String(),
			RewriteJob:    new(r.job.String()),
			ChargedBytes:  r.nextSize,
			SealedRecords: new(r.kept),
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (r *sealedReclaim) publishLocator(ctx context.Context, tx pgx.Tx, record hotRecord) error {
	encoded, err := json.Marshal(r.locators[record.receipt])
	if err != nil {
		return err
	}
	tag, err := dbgen.New(tx).
		StoreReclaimSealedFileUpdateTarget(ctx, dbgen.StoreReclaimSealedFileUpdateTargetParams{
			Attempt:       record.attempt.String(),
			Member:        r.member.id.String(),
			Incarnation:   r.nextID.String(),
			Locator:       encoded,
			Incarnation_2: r.source.file.String(),
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	_, err = dbgen.New(tx).StoreReclaimSealedFileUpdateReceipt(ctx, record.receipt.String())
	return err
}

func (r *sealedReclaim) publishRetirement(ctx context.Context, tx pgx.Tx) error {
	m := r.member
	tag, err := dbgen.New(tx).
		StoreReclaimSealedFileUpdateMemberFileRetiring(ctx, dbgen.StoreReclaimSealedFileUpdateMemberFileRetiringParams{
			Member:        m.id.String(),
			FileID:        r.source.file.String(),
			RewriteJob:    new(r.job.String()),
			Directory:     r.source.directory,
			ChargedBytes:  r.source.size,
			SealedRecords: new(r.source.count),
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	tag, err = dbgen.New(tx).
		StoreReclaimSealedFileUpdateHotRewrite(ctx, dbgen.StoreReclaimSealedFileUpdateHotRewriteParams{
			ID:          r.job.String(),
			Member:      m.id.String(),
			KeptRecords: new(r.kept),
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if err = commit(ctx, tx); err != nil {
		m.frozen = true
	}
	return err
}

func (r *sealedReclaim) finish(ctx context.Context) error {
	if err := r.member.checkRewriteFault("after_publish"); err != nil {
		r.member.frozen = true
		return err
	}
	if err := r.member.reconcileRewrites(ctx); err != nil {
		r.member.frozen = true
		return err
	}
	return nil
}

func sameSealedCensus(a, b []hotRecord) bool {
	if len(a) != len(b) {
		return false
	}
	for i, r := range a {
		if !sameSealedRecord(r, b[i]) {
			return false
		}
	}
	return true
}

func sameSealedRecord(a, b hotRecord) bool {
	return a.receipt == b.receipt && a.locator == b.locator && sameHotRewriteRecord(a, b)
}

type sealedReclaimJob struct {
	id, member, old, oldDir, newDir, state string
	next                                   *string
	kept, reserved                         int64
}

func loadSealedReclaimJob(ctx context.Context, pool *pgxpool.Pool, id string) (sealedReclaimJob, error) {
	var j sealedReclaimJob
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(pool).LoadSealedReclaimJob(ctx, id)
		if sqlcErr != nil {
			return sqlcErr
		}
		j.id = sqlcResult.ID
		j.member = sqlcResult.Member
		j.old = sqlcResult.OldIncarnation
		j.oldDir = sqlcResult.OldDirectory
		j.newDir = sqlcResult.NewDirectory
		j.state = sqlcResult.State
		j.next = sqlcResult.NewIncarnation
		if sqlcResult.KeptRecords == nil {
			return fmt.Errorf("receiptstore: unexpected NULL in LoadSealedReclaimJob.KeptRecords")
		}
		j.kept = *sqlcResult.KeptRecords
		j.reserved = sqlcResult.ReservedBytes
		return nil
	}()
	return j, err
}

func preflightSealedReclaimJob(ctx context.Context, pool *pgxpool.Pool, member ID, root, current, id string) error {
	j, err := loadSealedReclaimJob(ctx, pool, id)
	if err != nil {
		return err
	}
	if err = j.validateBinding(member, current); err != nil {
		return err
	}
	if j.state == "building" {
		return j.preflightBuilding(ctx, pool, member, root)
	}
	if j.state != "published" || j.reserved != 0 {
		return ErrConflict
	}
	if err = j.preflightPublishedSuccessor(ctx, pool, member, root); err != nil {
		return err
	}
	return verifyCatalogPath(ctx, pool, member, root, j.id, j.oldDir, &j.old, "retiring", true)
}

func (j sealedReclaimJob) validateBinding(member ID, current string) error {
	if j.member != member.String() || j.old == current || j.next != nil && *j.next == current {
		return ErrConflict
	}
	return j.validateDirectories()
}

func (j sealedReclaimJob) validateDirectories() error {
	if !validMemberDirectory(j.oldDir) || !validMemberDirectory(j.newDir) || j.oldDir == j.newDir || j.kept < 0 {
		return ErrConflict
	}
	if j.kept == 0 && (j.next != nil || j.reserved != 0) {
		return ErrConflict
	}
	return nil
}

func (j sealedReclaimJob) preflightBuilding(ctx context.Context, pool *pgxpool.Pool, member ID, root string) error {
	valid, err := dbgen.New(pool).
		PreflightSealedReclaimJobCheckMemberFile(ctx, dbgen.PreflightSealedReclaimJobCheckMemberFileParams{
			Member:    member.String(),
			FileID:    j.old,
			Directory: j.oldDir,
		})
	if err != nil {
		return err
	}
	if !valid {
		return ErrConflict
	}
	// A delete-only job never creates a replacement directory.
	if j.kept == 0 {
		return verifyAbsentSealedReplacement(ctx, pool, member, root, j)
	}
	return verifyCatalogPath(ctx, pool, member, root, j.id, j.newDir, j.next, "building", true)
}

func (j sealedReclaimJob) preflightPublishedSuccessor(
	ctx context.Context,
	pool *pgxpool.Pool,
	member ID,
	root string,
) error {
	if j.kept == 0 {
		return verifyAbsentSealedReplacement(ctx, pool, member, root, j)
	}
	if j.next == nil || *j.next == j.old {
		return ErrConflict
	}
	if err := verifyCatalogPath(ctx, pool, member, root, j.id, j.newDir, j.next, "sealed", false); err != nil {
		return err
	}
	count, err := dbgen.New(pool).
		PreflightSealedReclaimJobReadMemberFile(ctx, dbgen.PreflightSealedReclaimJobReadMemberFileParams{
			Member: member.String(),
			FileID: *j.next,
		})
	if err != nil {
		return err
	}
	if count == nil {
		return fmt.Errorf("receiptstore: unexpected NULL in PreflightSealedReclaimJobReadMemberFile.sealed_records")
	}
	if *count != j.kept {
		return ErrConflict
	}
	return nil
}

func (m *LocalMember) reconcileSealedReclaim(ctx context.Context, id string) error {
	if err := preflightSealedReclaimJob(ctx, m.pool, m.id, m.root, m.bytes.FileID().String(), id); err != nil {
		return err
	}
	j, err := loadSealedReclaimJob(ctx, m.pool, id)
	if err != nil {
		return err
	}
	if j.state == "building" {
		return m.discardBuildingSealedReclaim(ctx, j)
	}
	if err = m.removeGeneration(j.oldDir); err != nil {
		return err
	}
	if err = m.checkRewriteFault("after_unlink"); err != nil {
		return err
	}
	return m.finishRemovedSealedReclaim(ctx, j)
}

func (m *LocalMember) discardBuildingSealedReclaim(ctx context.Context, j sealedReclaimJob) error {
	if j.kept > 0 {
		if err := m.removeGeneration(j.newDir); err != nil {
			return err
		}
	}
	tx, err := (&authority{pool: m.pool}).begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if j.next != nil {
		if err = m.discardSealedSuccessor(ctx, tx, j); err != nil {
			return err
		}
	}
	tag, err := dbgen.New(tx).LocalMemberReconcileSealedReclaimUpdateHotRewrite(ctx, j.id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return commit(ctx, tx)
}

func (m *LocalMember) discardSealedSuccessor(ctx context.Context, tx pgx.Tx, j sealedReclaimJob) error {
	if _, err := dbgen.New(tx).
		LocalMemberReconcileSealedReclaimUpdateMemberFileLane(
			ctx,
			dbgen.LocalMemberReconcileSealedReclaimUpdateMemberFileLaneParams{
				Member: m.id.String(),
				FileID: *j.next,
			},
		); err != nil {
		return err
	}
	tag, err := dbgen.New(tx).
		LocalMemberReconcileSealedReclaimUpdateMemberFile(
			ctx,
			dbgen.LocalMemberReconcileSealedReclaimUpdateMemberFileParams{
				Member:     m.id.String(),
				FileID:     *j.next,
				RewriteJob: new(j.id),
			},
		)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (m *LocalMember) finishRemovedSealedReclaim(ctx context.Context, j sealedReclaimJob) error {
	tx, err := (&authority{pool: m.pool}).begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err = m.retireSealedReferences(ctx, tx, j); err != nil {
		return err
	}
	tag, err := dbgen.New(tx).
		LocalMemberReconcileSealedReclaimUpdateMemberFileRemoved(
			ctx,
			dbgen.LocalMemberReconcileSealedReclaimUpdateMemberFileRemovedParams{
				Member:     m.id.String(),
				FileID:     j.old,
				RewriteJob: new(j.id),
			},
		)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	tag, err = dbgen.New(tx).LocalMemberReconcileSealedReclaimUpdateHotRewriteRemoved(ctx, j.id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return commit(ctx, tx)
}

func (m *LocalMember) retireSealedReferences(ctx context.Context, tx pgx.Tx, j sealedReclaimJob) error {
	if _, err := dbgen.New(tx).
		LocalMemberReconcileSealedReclaimUpdateTarget(ctx, dbgen.LocalMemberReconcileSealedReclaimUpdateTargetParams{
			Member:      m.id.String(),
			Incarnation: j.old,
		}); err != nil {
		return err
	}
	if _, err := dbgen.New(tx).
		LocalMemberReconcileSealedReclaimUpdateCleanup(ctx, dbgen.LocalMemberReconcileSealedReclaimUpdateCleanupParams{
			Member:      m.id.String(),
			Incarnation: j.old,
		}); err != nil {
		return err
	}
	_, err := dbgen.New(tx).
		LocalMemberReconcileSealedReclaimUpdateMemberFileLane2(
			ctx,
			dbgen.LocalMemberReconcileSealedReclaimUpdateMemberFileLane2Params{
				Member: m.id.String(),
				FileID: j.old,
			},
		)
	return err
}

func verifyAbsentSealedReplacement(
	ctx context.Context,
	pool *pgxpool.Pool,
	member ID,
	root string,
	j sealedReclaimJob,
) error {
	if err := verifyCatalogPath(ctx, pool, member, root, j.id, j.newDir, nil, "building", true); err != nil {
		return err
	}
	_, err := storageLstat(root, j.newDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrConflict
}
