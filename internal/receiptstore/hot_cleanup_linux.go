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
	"strings"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func validMemberDirectory(dir string) bool {
	if dir == "." {
		return true
	}
	if !strings.HasPrefix(dir, "generation-") {
		return false
	}
	_, err := idFrom(strings.TrimPrefix(dir, "generation-"))
	return err == nil
}

func (m *LocalMember) removeGeneration(directory string) error {
	if !validMemberDirectory(directory) || directory == m.directory {
		return ErrConflict
	}
	var err error
	if directory == "." {
		err = withStorageRoot(m.root, func(root *os.Root) error { return root.Remove("data.fry") })
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	} else {
		err = withStorageRoot(m.root, func(root *os.Root) error { return root.RemoveAll(directory) })
	}
	if err != nil {
		return err
	}
	return syncMemberRoot(m.root)
}

// Called while owning the root and excluding all member operations. A process
// crash releases flock, so acquiring it on restart proves old local file handles
// from that process are gone. Database read pins remain separately conservative.
func (m *LocalMember) reconcileRewrites(ctx context.Context) error {
	if err := preflightMemberFiles(ctx, m.pool, m.id, m.root, m.opts); err != nil {
		return err
	}
	jobs, err := m.rewriteRecoveryJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if err = m.reconcileRewrite(ctx, job); err != nil {
			return err
		}
	}
	return nil
}

type hotRecord struct {
	attempt, receipt ID
	state            string
	version          int64
	keep             bool
	locator          hotstore.Locator
	lane             *int64
}

// snapshotHot locks attempts first, then receipts. A preparing attempt blocks
// rewriting: abort/reconcile it first. Actual I/O happens after this tx closes.
func (s *Store) snapshotHot(ctx context.Context, tx pgx.Tx, m *LocalMember) ([]hotRecord, error) {
	return s.snapshotHotFile(ctx, tx, m, m.bytes.FileID(), m.bytes.Inventory())
}

func (s *Store) snapshotHotFile(
	ctx context.Context,
	tx pgx.Tx,
	m *LocalMember,
	file ID,
	physical []hotstore.Locator,
) ([]hotRecord, error) {
	records, err := loadHotRecords(ctx, tx, m, file)
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].receipt.String() < records[j].receipt.String() })
	for i := range records {
		if err = s.populateHotRecord(ctx, tx, m, &records[i]); err != nil {
			return nil, err
		}
	}
	if err = matchHotInventory(ctx, tx, m, records, physical); err != nil {
		return nil, err
	}
	return records, nil
}

func loadHotRecords(ctx context.Context, tx pgx.Tx, m *LocalMember, file ID) ([]hotRecord, error) {
	rows, err := tx.Query(ctx, dbgen.StoreSnapshotHotFileReadTarget, m.id.String(), file.String())
	if err != nil {
		return nil, err
	}
	var records []hotRecord
	for rows.Next() {
		var record hotRecord
		record, err = scanHotRecord(rows, m, int64(len(records)))
		if err != nil {
			break
		}
		records = append(records, record)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	for _, record := range records {
		if record.state == "preparing" {
			return nil, ErrBusy
		}
	}
	return records, nil
}

func scanHotRecord(rows pgx.Rows, m *LocalMember, count int64) (hotRecord, error) {
	var record hotRecord
	var attempt, receipt string
	if err := rows.Scan(&attempt, &receipt, &record.state, &record.lane); err != nil {
		return record, err
	}
	var err error
	record.attempt, err = idFrom(attempt)
	if err != nil {
		return record, err
	}
	record.receipt, err = idFrom(receipt)
	if err != nil {
		return record, err
	}
	return record, validateHotRecordLane(m, record.lane, count)
}

func validateHotRecordLane(m *LocalMember, lane *int64, count int64) error {
	if m.opts.MaxRecords > 0 {
		if lane == nil || *lane < 0 || *lane >= memberCapacityLaneCount(m.opts) {
			return ErrConflict
		}
		if count >= m.opts.MaxRecords {
			return ErrConflict
		}
	}
	return nil
}

func (s *Store) populateHotRecord(ctx context.Context, tx pgx.Tx, m *LocalMember, record *hotRecord) error {
	if record.state == "aborted" {
		cancelled, err := dbgen.New(tx).
			StoreSnapshotHotFileReadTargetCancelConfirmed(
				ctx,
				dbgen.StoreSnapshotHotFileReadTargetCancelConfirmedParams{
					Attempt: record.attempt.String(),
					Member:  m.id.String(),
				},
			)
		if err != nil {
			return err
		}
		if !cancelled {
			return ErrBusy
		}
		return nil
	}
	if _, err := dbgen.New(tx).StoreSnapshotHotFileReadReceipt(ctx, record.receipt.String()); err != nil {
		return err
	}
	if _, err := s.reconcileLifetime(ctx, tx, record.receipt); err != nil {
		return err
	}
	row, err := dbgen.New(tx).StoreSnapshotHotFileReadReadPin(ctx, record.receipt.String())
	if err != nil {
		return err
	}
	record.version = row.ManifestVersion
	if row.Column2 == nil {
		return fmt.Errorf("receiptstore: unexpected NULL in StoreSnapshotHotFileReadReadPin.Column2")
	}
	record.keep = *row.Column2
	return nil
}

func matchHotInventory(
	ctx context.Context,
	tx pgx.Tx,
	m *LocalMember,
	records []hotRecord,
	physical []hotstore.Locator,
) error {
	inventory := map[hotstore.Key]hotstore.Locator{}
	for _, loc := range physical {
		inventory[loc.Key] = loc
	}
	for i, record := range records {
		key := hotstore.Key{Receipt: record.receipt, Attempt: record.attempt}
		loc, ok := inventory[key]
		if record.keep && !ok {
			return ErrConflict
		}
		if ok {
			if record.state == "accepted" {
				if err := verifyHotLocator(ctx, tx, m, record, loc); err != nil {
					return err
				}
			}
			records[i].locator = loc
			delete(inventory, key)
		}
	}
	if len(inventory) != 0 {
		return fmt.Errorf("%w: unregistered hot records require reconciliation", ErrConflict)
	}
	return nil
}

func verifyHotLocator(ctx context.Context, tx pgx.Tx, m *LocalMember, record hotRecord, actual hotstore.Locator) error {
	encoded, err := dbgen.New(tx).
		StoreSnapshotHotFileReadTargetLocator(ctx, dbgen.StoreSnapshotHotFileReadTargetLocatorParams{
			Attempt: record.attempt.String(),
			Member:  m.id.String(),
		})
	if err != nil {
		return err
	}
	var expected hotstore.Locator
	if err = json.Unmarshal(encoded, &expected); err != nil {
		return err
	}
	if actual != expected {
		return ErrConflict
	}
	return nil
}

// ReclaimHot rewrites a local or authenticated remote member, pausing reads and
// writes. Live or pinned records stay in that same failure domain. This operation
// rewrites only the current append file; ReclaimSealed handles immutable files.
// A failed/ambiguous publication leaves both files tracked.
func (s *Store) ReclaimHot(ctx context.Context, member ID) error {
	return s.dispatchMaintenance(ctx, member, maintenanceKindRewrite)
}

func (s *Store) reclaimMember(
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
	complete, err := m.beginMaintenance(ctx, operation, owner, expected)
	if err != nil || complete {
		return err
	}
	err = s.rewriteHot(ctx, m, operation, owner)
	if err != nil {
		if m.isFrozen() {
			return err
		}
		// Known prepublication failures may release admission only after tracked
		// scratch has been reconciled; no failed RPC is interpreted as I/O closure.
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
	if err = m.finishMaintenance(ctx, operation, maintenanceComplete); err != nil {
		m.frozen = true
		return err
	}
	return nil
}

func (s *Store) rewriteHot(ctx context.Context, m *LocalMember, operation ID, owner *memberOwner) (resultErr error) {
	r := hotRewrite{store: s, member: m, operation: operation, owner: owner}
	if err := m.reconcileRewrites(ctx); err != nil {
		return err
	}
	needed, err := r.reserve(ctx)
	if err != nil || !needed {
		return err
	}
	if err = r.openSuccessor(); err != nil {
		return err
	}
	defer r.closeUnretained(&resultErr)
	if err = r.buildSuccessor(ctx); err != nil {
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
	return r.activate(ctx)
}

type hotRewrite struct {
	store           *Store
	member          *LocalMember
	operation, job  ID
	owner           *memberOwner
	records         []hotRecord
	oldEncoded      int64
	directory, path string
	next            *hotstore.Store
	locators        map[ID]hotstore.Locator
	retained        bool
}

func (r *hotRewrite) reserve(ctx context.Context) (bool, error) {
	tx, err := r.store.db.begin(ctx)
	if err != nil {
		return false, err
	}
	needed, err := r.reserveTx(ctx, tx)
	if err != nil || !needed {
		rollback(ctx, tx)
		return false, err
	}
	return true, commit(ctx, tx)
}

func (r *hotRewrite) reserveTx(ctx context.Context, tx pgx.Tx) (bool, error) {
	m := r.member
	var err error
	r.records, err = r.store.snapshotHot(ctx, tx, m)
	if err != nil {
		return false, err
	}
	if !hasDroppedHotRecords(r.records) {
		return false, nil
	}
	r.job, err = hotstore.NewID()
	if err != nil {
		return false, err
	}
	r.oldEncoded, err = m.rewriteSourceSize()
	if err != nil {
		return false, err
	}
	charge, err := reserveRewriteCapacity(ctx, tx, m.id)
	if err != nil {
		return false, err
	}
	r.directory = "generation-" + r.job.String()
	_, err = dbgen.New(tx).
		StoreRewriteHotInsertHotRewrite(ctx, dbgen.StoreRewriteHotInsertHotRewriteParams{
			ID:             r.job.String(),
			Member:         m.id.String(),
			OldIncarnation: m.bytes.FileID().String(),
			OldDirectory:   m.directory,
			NewDirectory:   r.directory,
			ReservedBytes:  charge,
		})
	return true, err
}

func hasDroppedHotRecords(records []hotRecord) bool {
	for _, record := range records {
		if !record.keep {
			return true
		}
	}
	return false
}

func (m *LocalMember) rewriteSourceSize() (int64, error) {
	snapshot, err := m.bytes.Snapshot()
	if err != nil {
		return 0, err
	}
	size := snapshot.Size()
	if err = closeSealedImage(m, snapshot); err != nil {
		return 0, err
	}
	return size, nil
}

func (r *hotRewrite) openSuccessor() error {
	m := r.member
	r.path = filepath.Join(m.root, r.directory)
	if err := os.Mkdir(r.path, 0o700); err != nil {
		return err
	}
	if err := syncMemberRoot(m.root); err != nil {
		return err
	}
	nextOptions := m.opts
	nextOptions.MaxBytes -= r.oldEncoded
	if nextOptions.MaxBytes < 64 {
		return hotstore.ErrCapacity
	}
	var err error
	r.next, err = hotstore.Open(r.path, nextOptions)
	return err
}

func (r *hotRewrite) closeUnretained(resultErr *error) {
	if !r.retained {
		*resultErr = errors.Join(*resultErr, closeMemberStore(r.member, r.next))
	}
}

func (r *hotRewrite) buildSuccessor(ctx context.Context) error {
	m := r.member
	if err := m.checkRewriteFault("before_register"); err != nil {
		return err
	}
	if err := m.registerSuccessor(ctx, r.job, r.directory, r.next.FileID()); err != nil {
		return err
	}
	if err := r.copyRecords(ctx); err != nil {
		return err
	}
	return m.checkRewriteFault("after_copy")
}

func (r *hotRewrite) copyRecords(ctx context.Context) error {
	r.locators = map[ID]hotstore.Locator{}
	// Preserve physical append order for byte-cap admission. Each copied prefix
	// is a subset of an admitted prefix, including next-record headroom.
	order := append([]hotRecord(nil), r.records...)
	sort.Slice(order, func(i, j int) bool { return order[i].locator.Offset < order[j].locator.Offset })
	for _, record := range order {
		if !record.keep {
			continue
		}
		loc, err := copyHotRecord(ctx, r.next, record.locator, r.member.bytes.Read)
		if err != nil {
			return err
		}
		r.locators[record.receipt] = loc
	}
	return nil
}

func (r *hotRewrite) publish(ctx context.Context, tx pgx.Tx) error {
	if err := r.checkPublication(ctx, tx); err != nil {
		return err
	}
	if err := r.publishLocators(ctx, tx); err != nil {
		return err
	}
	return r.publishCatalog(ctx, tx)
}

func (r *hotRewrite) checkPublication(ctx context.Context, tx pgx.Tx) error {
	m := r.member
	current, err := r.store.snapshotHot(ctx, tx, m)
	if err != nil {
		return err
	}
	if !sameHotRewriteSnapshot(current, r.records) {
		return ErrStale
	}
	binding, err := dbgen.New(tx).StoreRewriteHotReadMember(ctx, m.id.String())
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
	return setSuccessorCapacity(ctx, tx, m.id, r.next.FileID(), r.job, m.opts, r.records, r.locators)
}

func sameHotRewriteSnapshot(current, original []hotRecord) bool {
	if len(current) != len(original) {
		return false
	}
	for i, record := range current {
		if !sameHotRewriteRecord(record, original[i]) {
			return false
		}
	}
	return true
}

func sameHotRewriteRecord(a, b hotRecord) bool {
	if a.attempt != b.attempt || a.state != b.state || a.version != b.version || a.keep != b.keep {
		return false
	}
	if (a.lane == nil) != (b.lane == nil) {
		return false
	}
	return a.lane == nil || *a.lane == *b.lane
}

func (r *hotRewrite) publishLocators(ctx context.Context, tx pgx.Tx) error {
	for _, record := range r.records {
		if !record.keep {
			continue
		}
		loc := r.locators[record.receipt]
		encoded, err := json.Marshal(loc)
		if err != nil {
			return err
		}
		if _, err = dbgen.New(tx).
			StoreRewriteHotUpdateTarget(ctx, dbgen.StoreRewriteHotUpdateTargetParams{
				Attempt:     record.attempt.String(),
				Member:      r.member.id.String(),
				Incarnation: loc.File.String(),
				Locator:     encoded,
			}); err != nil {
			return err
		}
		if _, err = dbgen.New(tx).StoreRewriteHotUpdateReceipt(ctx, record.receipt.String()); err != nil {
			return err
		}
	}
	return nil
}

func (r *hotRewrite) publishCatalog(ctx context.Context, tx pgx.Tx) error {
	m := r.member
	tag, err := dbgen.New(tx).
		StoreRewriteHotUpdateMemberFile(ctx, dbgen.StoreRewriteHotUpdateMemberFileParams{
			Member:       m.id.String(),
			FileID:       m.bytes.FileID().String(),
			RewriteJob:   new(r.job.String()),
			Directory:    m.directory,
			ChargedBytes: r.oldEncoded,
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	tag, err = dbgen.New(tx).
		StoreRewriteHotUpdateMemberFileCurrent(ctx, dbgen.StoreRewriteHotUpdateMemberFileCurrentParams{
			Member:     m.id.String(),
			FileID:     r.next.FileID().String(),
			RewriteJob: new(r.job.String()),
			Directory:  r.directory,
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err = dbgen.New(tx).
		StoreRewriteHotUpdateMember(ctx, dbgen.StoreRewriteHotUpdateMemberParams{
			ID:          m.id.String(),
			Incarnation: r.next.FileID().String(),
			Directory:   r.directory,
		}); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).StoreRewriteHotUpdateHotRewrite(ctx, r.job.String()); err != nil {
		return err
	}
	if err = commit(ctx, tx); err != nil {
		return r.fenceSource(err)
	}
	return nil
}

func (r *hotRewrite) fenceSource(err error) error {
	r.member.frozen = true
	return errors.Join(err, closeMemberStore(r.member, r.member.bytes))
}

func (r *hotRewrite) activate(ctx context.Context) error {
	m := r.member
	if err := m.checkRewriteFault("after_publish"); err != nil {
		return r.fenceSource(err)
	}
	old := m.bytes
	m.bytes, m.directory, r.retained = r.next, r.directory, true
	if err := closeMemberStore(m, old); err != nil {
		return err
	}
	if err := m.reconcileRewrites(ctx); err != nil {
		m.frozen = true
		return err
	}
	// Restore the full single-file budget only after old occupancy is gone.
	if err := closeMemberStore(m, r.next); err != nil {
		return err
	}
	reopened, err := hotstore.Open(r.path, m.opts)
	if err != nil {
		m.frozen = true
		return err
	}
	m.bytes = reopened
	return nil
}

// FinishCleanup records removal only after every registered hot/object copy and
// actual read has been resolved. It does not erase receipt/attempt audit history.
// Each call examines at most limit candidates in one process-local scan cycle,
// including blocked candidates. Independent stores may select the same work.
func (s *Store) FinishCleanup(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1024 {
		return 0, fmt.Errorf("receiptstore: invalid cleanup limit")
	}
	finished := 0
	var cycle *cleanupCycle
	for range limit {
		c, selectedCycle, end, e := s.selectCleanupCandidate(ctx, cycle)
		if e != nil {
			return finished, e
		}
		if c == nil {
			break
		}
		cycle = selectedCycle
		removed, e := s.finishCleanupCandidate(ctx, c)
		if e != nil {
			return finished, e
		}
		if removed {
			finished++
		}
		if end {
			break
		}
	}
	return finished, nil
}

// cleanupCycle is scheduling state only. Each caller stays in the cycle it
// joined, even when another caller finishes that cycle during its receipt tx.
type cleanupPosition struct {
	due time.Time
	id  string
}
type cleanupCycle struct {
	high  cleanupPosition
	after *cleanupPosition
}
type cleanupCandidate struct{ id, org ID }

// selectCleanupCandidate advances exactly one position before any receipt locks.
// The upper endpoint is frozen; arrivals behind the cursor or above
// the high water wait for the next pass. No cleanup authority is changed here.
func (s *Store) selectCleanupCandidate(
	ctx context.Context,
	joined *cleanupCycle,
) (*cleanupCandidate, *cleanupCycle, bool, error) {
	select {
	case s.cleanupSelection <- struct{}{}:
		defer func() { <-s.cleanupSelection }()
	case <-ctx.Done():
		return nil, joined, false, ctx.Err()
	}
	if joined != nil && s.cleanupCycle != joined {
		return nil, joined, true, nil
	}
	if s.cleanupCycle == nil {
		empty, err := s.initializeCleanupCycle(ctx)
		if err != nil {
			return nil, nil, false, err
		}
		if empty {
			return nil, nil, true, nil
		}
	}
	cycle := s.cleanupCycle
	row, err := cycle.next(ctx, s.db.pool)
	if errors.Is(err, pgx.ErrNoRows) {
		s.cleanupCycle = nil
		return nil, cycle, true, nil
	}
	if err != nil {
		return nil, cycle, false, err
	}
	return s.advanceCleanupCycle(cycle, row)
}

type rewriteRecoveryJob struct {
	id, inc, old, new, state, kind string
	next                           *string
}

func (m *LocalMember) rewriteRecoveryJobs(ctx context.Context) ([]rewriteRecoveryJob, error) {
	rows, err := m.pool.Query(ctx, dbgen.LocalMemberReconcileRewritesReadHotRewrite, m.id.String())
	if err != nil {
		return nil, err
	}
	var jobs []rewriteRecoveryJob
	for rows.Next() {
		var j rewriteRecoveryJob
		if err = rows.Scan(&j.id, &j.inc, &j.old, &j.new, &j.state, &j.next, &j.kind); err != nil {
			break
		}
		jobs = append(jobs, j)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	return jobs, nil
}

func (m *LocalMember) reconcileRewrite(ctx context.Context, job rewriteRecoveryJob) error {
	if job.kind == "sealed-reclaim" {
		return m.reconcileSealedReclaim(ctx, job.id)
	}
	if job.state == "building" {
		return m.reconcileBuildingRewrite(ctx, job)
	}
	if job.kind == "rotate" {
		return m.reconcilePublishedRotation(ctx, job)
	}
	return m.reconcilePublishedRewrite(ctx, job)
}

func (m *LocalMember) reconcileBuildingRewrite(ctx context.Context, job rewriteRecoveryJob) error {
	if err := verifyCatalogPath(ctx, m.pool, m.id, m.root, job.id, job.new, job.next, "building", true); err != nil {
		return err
	}
	if err := m.removeGeneration(job.new); err != nil {
		return err
	}
	tx, err := (&authority{pool: m.pool}).begin(ctx)
	if err != nil {
		return err
	}
	if job.next != nil {
		_, err = dbgen.New(tx).
			LocalMemberReconcileRewritesUpdateMemberFileLane(
				ctx,
				dbgen.LocalMemberReconcileRewritesUpdateMemberFileLaneParams{
					Member: m.id.String(),
					FileID: *job.next,
				},
			)
	}
	if job.next != nil && err == nil {
		_, err = dbgen.New(tx).
			LocalMemberReconcileRewritesUpdateMemberFile(ctx, dbgen.LocalMemberReconcileRewritesUpdateMemberFileParams{
				Member:     m.id.String(),
				FileID:     *job.next,
				RewriteJob: new(job.id),
			})
	}
	if err == nil {
		_, err = dbgen.New(tx).LocalMemberReconcileRewritesUpdateHotRewrite(ctx, job.id)
	}
	if err == nil {
		err = commit(ctx, tx)
	} else {
		rollback(ctx, tx)
	}
	return err
}

func (m *LocalMember) validRewriteSuccessor(job rewriteRecoveryJob) bool {
	return job.next != nil && *job.next == m.bytes.FileID().String() && m.directory == job.new
}

func (m *LocalMember) reconcilePublishedRotation(ctx context.Context, job rewriteRecoveryJob) error {
	if !m.validRewriteSuccessor(job) {
		return ErrConflict
	}
	old := job.inc
	if err := verifyCatalogPath(ctx, m.pool, m.id, m.root, job.id, job.old, &old, "sealed", false); err != nil {
		return err
	}
	tx, err := (&authority{pool: m.pool}).begin(ctx)
	if err != nil {
		return err
	}
	tag, err := dbgen.New(tx).
		LocalMemberReconcileRewritesUpdateHotRewriteComplete(
			ctx,
			dbgen.LocalMemberReconcileRewritesUpdateHotRewriteCompleteParams{
				ID:     job.id,
				Member: m.id.String(),
			},
		)
	if err != nil {
		rollback(ctx, tx)
		return err
	}
	if tag.RowsAffected() != 1 {
		rollback(ctx, tx)
		return ErrConflict
	}
	return commit(ctx, tx)
}

func (m *LocalMember) reconcilePublishedRewrite(ctx context.Context, job rewriteRecoveryJob) error {
	if !m.validRewriteSuccessor(job) {
		return ErrConflict
	}
	old := job.inc
	if err := verifyCatalogPath(ctx, m.pool, m.id, m.root, job.id, job.old, &old, "retiring", true); err != nil {
		return err
	}
	if err := m.removeGeneration(job.old); err != nil {
		return err
	}
	if err := m.checkRewriteFault("after_unlink"); err != nil {
		return err
	}
	return m.finishRemovedHotRewrite(ctx, job)
}

func (m *LocalMember) finishRemovedHotRewrite(ctx context.Context, job rewriteRecoveryJob) error {
	tx, err := (&authority{pool: m.pool}).begin(ctx)
	if err != nil {
		return err
	}
	_, err = dbgen.New(tx).
		LocalMemberReconcileRewritesUpdateTarget(ctx, dbgen.LocalMemberReconcileRewritesUpdateTargetParams{
			Member:      m.id.String(),
			Incarnation: job.inc,
		})
	if err == nil {
		_, err = dbgen.New(tx).
			LocalMemberReconcileRewritesUpdateCleanup(ctx, dbgen.LocalMemberReconcileRewritesUpdateCleanupParams{
				Member:      m.id.String(),
				Incarnation: job.inc,
			})
	}
	if err == nil {
		_, err = dbgen.New(tx).
			LocalMemberReconcileRewritesUpdateMemberFileLane2(
				ctx,
				dbgen.LocalMemberReconcileRewritesUpdateMemberFileLane2Params{
					Member: m.id.String(),
					FileID: job.inc,
				},
			)
	}
	if err == nil {
		_, err = dbgen.New(tx).
			LocalMemberReconcileRewritesUpdateMemberFileRemoved(
				ctx,
				dbgen.LocalMemberReconcileRewritesUpdateMemberFileRemovedParams{
					Member:     m.id.String(),
					FileID:     job.inc,
					RewriteJob: new(job.id),
				},
			)
	}
	if err == nil {
		_, err = dbgen.New(tx).LocalMemberReconcileRewritesUpdateHotRewriteRemoved(ctx, job.id)
	}
	if err == nil {
		err = commit(ctx, tx)
	} else {
		rollback(ctx, tx)
	}
	return err
}

func (s *Store) finishCleanupCandidate(ctx context.Context, candidate *cleanupCandidate) (bool, error) {
	removed := false
	tx, err := s.db.begin(ctx)
	if err != nil {
		return false, err
	}
	_, err = lockReceipt(ctx, tx, candidate.org, candidate.id)
	if err == nil {
		_, err = dbgen.New(tx).StoreFinishCleanupDeleteHeaderMetadata(ctx, candidate.id.String())
	}
	if err == nil {
		var row dbgen.StoreFinishCleanupUpdateReceiptRow
		row, err = dbgen.New(tx).StoreFinishCleanupUpdateReceipt(ctx, candidate.id.String())
		if errors.Is(err, pgx.ErrNoRows) {
			err = nil
		} else if err == nil {
			var org ID
			org, err = idFrom(row.Organization)
			if err == nil {
				err = removeRetainedStorage(ctx, tx, org, candidate.id, row.RawSize)
			}
			removed = err == nil
		}
	}
	if err == nil {
		err = commit(ctx, tx)
	} else {
		rollback(ctx, tx)
	}
	return removed, err
}

func (s *Store) initializeCleanupCycle(ctx context.Context) (bool, error) {
	row, err := dbgen.New(s.db.pool).StoreSelectCleanupCandidateReadReceipt(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if row.CleanupDue == nil {
		return false, fmt.Errorf("receiptstore: retiring receipt %s has no cleanup deadline", row.ID)
	}
	s.cleanupCycle = &cleanupCycle{high: cleanupPosition{due: *row.CleanupDue, id: row.ID}}
	return false, nil
}

type cleanupSelectionRow struct {
	id, org string
	due     time.Time
}

func (cycle *cleanupCycle) next(ctx context.Context, pool *pgxpool.Pool) (cleanupSelectionRow, error) {
	if cycle.after == nil {
		row, err := dbgen.New(pool).
			StoreSelectCleanupCandidateReadReceiptRetiring(
				ctx,
				dbgen.StoreSelectCleanupCandidateReadReceiptRetiringParams{
					Column1: cycle.high.due,
					Column2: cycle.high.id,
				},
			)
		if err != nil {
			return cleanupSelectionRow{}, err
		}
		if row.CleanupDue == nil {
			return cleanupSelectionRow{}, fmt.Errorf(
				"receiptstore: unexpected NULL in StoreSelectCleanupCandidateReadReceiptRetiring.CleanupDue",
			)
		}
		return cleanupSelectionRow{row.ID, row.Organization, *row.CleanupDue}, nil
	}
	row, err := dbgen.New(pool).
		StoreSelectCleanupCandidateReadReceiptRetiringIdOrganizationCleanupDue(
			ctx,
			dbgen.StoreSelectCleanupCandidateReadReceiptRetiringIdOrganizationCleanupDueParams{
				Column1: cycle.after.due,
				Column2: cycle.after.id,
				Column3: cycle.high.due,
				Column4: cycle.high.id,
			},
		)
	if err != nil {
		return cleanupSelectionRow{}, err
	}
	if row.CleanupDue == nil {
		return cleanupSelectionRow{}, fmt.Errorf(
			"receiptstore: unexpected NULL in StoreSelectCleanupCandidateReadReceip" +
				"tRetiringIdOrganizationCleanupDue.CleanupDue",
		)
	}
	return cleanupSelectionRow{row.ID, row.Organization, *row.CleanupDue}, nil
}

func (s *Store) advanceCleanupCycle(
	cycle *cleanupCycle,
	row cleanupSelectionRow,
) (*cleanupCandidate, *cleanupCycle, bool, error) {
	receipt, err := idFrom(row.id)
	if err != nil {
		return nil, cycle, false, err
	}
	organization, err := idFrom(row.org)
	if err != nil {
		return nil, cycle, false, err
	}
	cycle.after = &cleanupPosition{due: row.due, id: row.id}
	end := row.id == cycle.high.id && row.due.Equal(cycle.high.due)
	if end {
		s.cleanupCycle = nil
	}
	return &cleanupCandidate{id: receipt, org: organization}, cycle, end, nil
}
