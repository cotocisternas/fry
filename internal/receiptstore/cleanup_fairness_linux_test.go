//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func cleanupOriginal(t *testing.T, f *fixture) Receipt {
	t.Helper()
	r := accept(t, f.s, newID(t), f.envelope(t), "Subject: cleanup fairness\r\n\r\nsynthetic original")
	{
		_, err := f.s.Header(context.Background(), f.org, r.ID)
		requireReceiptCondition(t, err == nil, err)
	}
	return r
}

func cleanupExpireAt(t *testing.T, f *fixture, at time.Time, receipts ...Receipt) {
	t.Helper()
	for _, r := range receipts {
		execSQL(t, f.pool, `UPDATE fry_reference SET expires_at=$2 WHERE receipt=$1`, r.ID.String(), at)
	}
	{
		_, err := f.s.SweepExpiry(context.Background(), 1024)
		requireReceiptCondition(t, err == nil, err)
	}
}

func cleanupReclaim(t *testing.T, f *fixture) {
	t.Helper()
	for _, m := range f.members {
		requireReceiptNoError(t, f.s.ReclaimHot(context.Background(), m.id))
	}
}

func cleanupWantState(t *testing.T, f *fixture, r Receipt, want string) {
	t.Helper()
	status, err := f.s.CleanupStatus(context.Background(), f.org, r.ID)
	requireReceiptCondition(t, err == nil, "cleanup state", r.ID, status, err, "want", want)
	requireReceiptCondition(t, status.State == want, "cleanup state", r.ID, status, err, "want", want)
}

func cleanupWaitLock(t *testing.T, f *fixture) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for scalar(t, f.pool, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND `+
		`wait_event_type='Lock'`, f.pool.Config().ConnConfig.RuntimeParams["application_name"]) == 0 {
		requireReceiptCondition(t, !time.Now().After(deadline), "cleanup did not reach held receipt lock")
		time.Sleep(5 * time.Millisecond)
	}
}

func cleanupIndependent(t *testing.T, f *fixture) *Store {
	t.Helper()
	s, err := New(context.Background(), f.pool, f.members, f.s.db.opts)
	requireReceiptNoError(t, err)
	return s
}

func TestCleanupFairnessBlockedHotOldestDeletesHeaderAndVisitsLater(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	base := time.Now().Add(-72 * time.Hour)
	a, b := cleanupOriginal(t, f), cleanupOriginal(t, f)
	cleanupExpireAt(t, f, base.Add(time.Second), b)
	cleanupReclaim(t, f) // A is live, so its actual hot copies survive this rewrite.
	cleanupExpireAt(t, f, base, a)
	{
		n, err := f.s.FinishCleanup(ctx, 1)
		requireReceiptCondition(t, n == 0, n, err)
		requireReceiptCondition(t, err == nil, n, err)
	}
	cleanupWantState(t, f, a, "retiring")
	{
		n := scalar(t, f.pool, `SELECT count(*) FROM fry_header_metadata WHERE receipt=$1`, a.ID.String())
		requireReceiptCondition(t, n == 0, "blocked candidate retained header", n)
	}
	{
		n := scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE attempt=$1 AND NOT bytes_removed`,
			a.Attempt.String(),
		)
		requireReceiptCondition(t, n == 2, "hot authority was bypassed", n)
	}
	{
		n, err := f.s.FinishCleanup(ctx, 1)
		requireReceiptCondition(t, n == 1, "oldest starved removable receipt", n, err)
		requireReceiptCondition(t, err == nil, "oldest starved removable receipt", n, err)
	}
	cleanupWantState(t, f, b, "removed")
}

func TestCleanupFairnessActualReadPinPreservesHotCopies(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a, b := cleanupOriginal(t, f), cleanupOriginal(t, f)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.s.beforeReadAttempt = func(ID) { once.Do(func() { close(entered) }); <-release }
	releaseRead := sync.OnceFunc(func() { close(release) })
	defer releaseRead()
	handle, err := f.s.Open(ctx, f.org, a.ID, 0, a.Size)
	requireReceiptNoError(t, err)
	defer func() {
		releaseRead()
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("pinned read did not reach pre-attempt barrier")
	}
	base := time.Now().Add(-72 * time.Hour)
	cleanupExpireAt(t, f, base, a)
	cleanupExpireAt(t, f, base.Add(time.Second), b)
	cleanupReclaim(t, f) // The genuine Open pin retains A without a fabricated pin row.
	{
		n, err := f.s.FinishCleanup(ctx, 1)
		requireReceiptCondition(t, n == 0, n, err)
		requireReceiptCondition(t, err == nil, n, err)
	}
	requirePinnedCleanupDebt(t, f, a)
	{
		n, err := f.s.FinishCleanup(ctx, 1)
		requireReceiptCondition(t, n == 1, "pinned oldest starved later cleanup", n, err)
		requireReceiptCondition(t, err == nil, "pinned oldest starved later cleanup", n, err)
	}
	releaseRead()
	if err := handle.Close(); err != nil {
		t.Error(err)
	}
	cleanupReclaim(t, f)
	{
		n, err := f.s.FinishCleanup(ctx, 2)
		requireReceiptCondition(t, n == 1, "closed pin did not become removable", n, err)
		requireReceiptCondition(t, err == nil, "closed pin did not become removable", n, err)
	}
	cleanupWantState(t, f, a, "removed")
}

func TestCleanupFairnessTimeoutAdvancesOnlyOneCandidate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	base := time.Now().Add(-72 * time.Hour)
	a, b := cleanupOriginal(t, f), cleanupOriginal(t, f)
	cleanupExpireAt(t, f, base, a)
	cleanupExpireAt(t, f, base.Add(time.Second), b)
	cleanupReclaim(t, f)
	tx, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}()
	_, err = tx.Exec(ctx, `SELECT id FROM fry_receipt WHERE id=$1 FOR UPDATE`, a.ID.String())
	requireReceiptNoError(t, err)
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() { n, err := f.s.FinishCleanup(bounded, 8); done <- result{n, err} }()
	cleanupWaitLock(t, f) // Proves selection advanced to A before cancellation.
	cancel()
	select {
	case result := <-done:
		requireReceiptCondition(t, result.n == 0, "held oldest ignored cancellation", result)
		requireReceiptCondition(t, result.err != nil, "held oldest ignored cancellation", result)
	case <-time.After(3 * time.Second):
		t.Fatal("blocked cleanup did not honor cancellation")
	}
	// A failed transaction must not advance a preselected page past B.
	healthy, healthyCancel := context.WithTimeout(ctx, 3*time.Second)
	defer healthyCancel()
	{
		n, err := f.s.FinishCleanup(healthy, 8)
		requireReceiptCondition(t, n == 1, "unattempted suffix was skipped", n, err)
		requireReceiptCondition(t, err == nil, "unattempted suffix was skipped", n, err)
	}
	cleanupWantState(t, f, b, "removed")
	requireReceiptNoError(t, tx.Rollback(ctx))
	{
		n, err := f.s.FinishCleanup(healthy, 8)
		requireReceiptCondition(t, n == 1, "retry cycle failed", n, err)
		requireReceiptCondition(t, err == nil, "retry cycle failed", n, err)
	}
}

func TestCleanupFairnessConcurrentCycleEndDoesNotWrapCaller(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	base := time.Now().Add(-72 * time.Hour)
	a, b := cleanupOriginal(t, f), cleanupOriginal(t, f)
	cleanupExpireAt(t, f, base.Add(time.Second), b)
	cleanupReclaim(t, f)
	cleanupExpireAt(t, f, base, a)
	tx, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}()
	_, err = tx.Exec(ctx, `SELECT id FROM fry_receipt WHERE id=$1 FOR UPDATE`, a.ID.String())
	requireReceiptNoError(t, err)
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	type result struct {
		n   int
		err error
	}
	first := make(chan result, 1)
	go func() { n, err := f.s.FinishCleanup(bounded, 8); first <- result{n, err} }()
	cleanupWaitLock(t, f)
	// The selection gate is free while call1 waits for A's receipt lock. Call2
	// selects final B, closes that cycle, and commits before A is unlocked.
	{
		n, err := f.s.FinishCleanup(bounded, 1)
		requireReceiptCondition(t, n == 1, "selection gate held during receipt tx", n, err)
		requireReceiptCondition(t, err == nil, "selection gate held during receipt tx", n, err)
	}
	c := cleanupOriginal(t, f)
	cleanupExpireAt(t, f, base.Add(2*time.Second), c)
	requireReceiptNoError(t, tx.Rollback(ctx))
	select {
	case result := <-first:
		requireReceiptCondition(t, result.n == 0, result)
		requireReceiptCondition(t, result.err == nil, result)
	case <-bounded.Done():
		t.Fatal("first cleanup did not finish")
	}
	// C is in a newer cycle: call1 must not visit it even with unused limit.
	{
		n := scalar(t, f.pool, `SELECT count(*) FROM fry_header_metadata WHERE receipt=$1`, c.ID.String())
		requireReceiptCondition(t, n == 1, "in-flight caller wrapped into newer cycle", n)
	}
	cleanupWantState(t, f, a, "retiring")
	cleanupWantState(t, f, c, "retiring")
}

func TestCleanupFairnessFrozenHighWaterRevisitsRepairAmidArrivals(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	base := time.Now().Add(-72 * time.Hour)
	a, b, c := cleanupOriginal(t, f), cleanupOriginal(t, f), cleanupOriginal(t, f)
	cleanupExpireAt(t, f, base.Add(time.Second), b)
	cleanupExpireAt(t, f, base.Add(2*time.Second), c)
	cleanupReclaim(t, f)
	cleanupExpireAt(t, f, base, a)
	{
		n, err := f.s.FinishCleanup(ctx, 1)
		requireReceiptCondition(t, n == 0, n, err)
		requireReceiptCondition(t, err == nil, n, err)
	} // Freeze high=C; A is blocked.
	cleanupReclaim(t, f) // Repair A's actual hot-copy blocker.
	d := cleanupOriginal(t, f)
	cleanupExpireAt(t, f, base.Add(3*time.Second), d)
	cleanupReclaim(t, f)
	{
		n, err := f.s.FinishCleanup(ctx, 100)
		requireReceiptCondition(t, n == 2, "call crossed frozen high water", n, err)
		requireReceiptCondition(t, err == nil, "call crossed frozen high water", n, err)
	}
	cleanupWantState(t, f, a, "retiring")
	cleanupWantState(t, f, d, "retiring")
	e := cleanupOriginal(t, f)
	cleanupExpireAt(t, f, base.Add(4*time.Second), e)
	cleanupReclaim(t, f)
	{
		n, err := f.s.FinishCleanup(ctx, 1)
		requireReceiptCondition(t, n == 1, "arrivals starved repaired oldest", n, err)
		requireReceiptCondition(t, err == nil, "arrivals starved repaired oldest", n, err)
	}
	cleanupWantState(t, f, a, "removed")
	cleanupWantState(t, f, d, "retiring")
	cleanupWantState(t, f, e, "retiring")
}

func TestCleanupFairnessTiesRemovedEndAndArrivalBehindCursor(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	base := time.Now().Add(-72 * time.Hour)
	receipts := []Receipt{cleanupOriginal(t, f), cleanupOriginal(t, f), cleanupOriginal(t, f)}
	cleanupExpireAt(t, f, base, receipts...)
	cleanupReclaim(t, f)
	sort.Slice(receipts, func(i, j int) bool { return receipts[i].ID.String() < receipts[j].ID.String() })
	{
		n, err := f.s.FinishCleanup(ctx, 1)
		requireReceiptCondition(t, n == 1, n, err)
		requireReceiptCondition(t, err == nil, n, err)
	}
	cleanupWantState(t, f, receipts[0], "removed")
	cleanupWantState(t, f, receipts[1], "retiring")
	cleanupWantState(t, f, receipts[2], "retiring")
	independent := cleanupIndependent(t, f)
	{
		n, err := independent.FinishCleanup(ctx, 100)
		requireReceiptCondition(t, n == 2, n, err)
		requireReceiptCondition(t, err == nil, n, err)
	} // Removes stored high row too.
	behind, later := cleanupOriginal(t, f), cleanupOriginal(t, f)
	cleanupExpireAt(t, f, base.Add(-time.Hour), behind)
	cleanupExpireAt(t, f, base.Add(time.Hour), later)
	cleanupReclaim(t, f)
	{
		n, err := f.s.FinishCleanup(ctx, 100)
		requireReceiptCondition(t, n == 0, "missing end wrapped inside same call", n, err)
		requireReceiptCondition(t, err == nil, "missing end wrapped inside same call", n, err)
	}
	cleanupWantState(t, f, behind, "retiring")
	cleanupWantState(t, f, later, "retiring")
	{
		n, err := f.s.FinishCleanup(ctx, 1)
		requireReceiptCondition(t, n == 1, n, err)
		requireReceiptCondition(t, err == nil, n, err)
	}
	cleanupWantState(t, f, behind, "removed")
	cleanupWantState(t, f, later, "retiring")
	{
		n, err := f.s.FinishCleanup(ctx, 100)
		requireReceiptCondition(t, n == 1, n, err)
		requireReceiptCondition(t, err == nil, n, err)
	}
}

func TestCleanupFairnessIndependentStoresCountTransitionsOnce(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var receipts []Receipt
	for range 10 {
		receipts = append(receipts, cleanupOriginal(t, f))
	}
	cleanupExpireAt(t, f, time.Now().Add(-72*time.Hour), receipts...)
	cleanupReclaim(t, f)
	stores := []*Store{f.s, cleanupIndependent(t, f)}
	var wg sync.WaitGroup
	var removed atomic.Int64
	for worker := range 4 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			for range 12 {
				n, err := stores[index%2].FinishCleanup(ctx, 2)
				if err != nil {
					t.Error(err)
					return
				}
				removed.Add(int64(n))
			}
		}(worker)
	}
	wg.Wait()
	requireReceiptCondition(t, removed.Load() == 10, "duplicate committed transition count", removed.Load())
	{
		n := scalar(t, f.pool, `SELECT count(*) FROM fry_receipt WHERE lifecycle='removed'`)
		requireReceiptCondition(t, n == 10, n)
	}
	{
		n := scalar(t, f.pool, `SELECT count(*) FROM fry_header_metadata`)
		requireReceiptCondition(t, n == 0, "removed metadata retained", n)
	}
}

func TestCleanupFairnessCommitFailureDoesNotCountOrDeleteMetadata(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r := cleanupOriginal(t, f)
	cleanupExpireAt(t, f, time.Now().Add(-72*time.Hour), r)
	cleanupReclaim(t, f)
	execSQL(
		t,
		f.pool,
		`CREATE FUNCTION fail_cleanup_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF `+
			`NEW.lifecycle='removed' THEN RAISE EXCEPTION 'synthetic deferred cleanup failure'; `+
			`END IF; RETURN NEW; END $$`,
	)
	execSQL(
		t,
		f.pool,
		`CREATE CONSTRAINT TRIGGER fail_cleanup_commit AFTER UPDATE ON fry_receipt DEFERRABLE `+
			`INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_cleanup_commit()`,
	)
	{
		n, err := f.s.FinishCleanup(ctx, 1)
		requireReceiptCondition(t, n == 0, "failed commit counted removal", n, err)
		requireReceiptCondition(t, errors.Is(err, ErrUncertain), "failed commit counted removal", n, err)
	}
	cleanupWantState(t, f, r, "retiring")
	{
		n := scalar(t, f.pool, `SELECT count(*) FROM fry_header_metadata WHERE receipt=$1`, r.ID.String())
		requireReceiptCondition(t, n == 1, "failed transaction deleted metadata", n)
	}
	execSQL(t, f.pool, `DROP TRIGGER fail_cleanup_commit ON fry_receipt`)
	{
		n, err := f.s.FinishCleanup(ctx, 1)
		requireReceiptCondition(t, n == 1, n, err)
		requireReceiptCondition(t, err == nil, n, err)
	}
}

func TestCleanupFairnessSelectionGateHonorsContext(t *testing.T) {
	s := &Store{cleanupSelection: make(chan struct{}, 1)}
	s.cleanupSelection <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	{
		n, err := s.FinishCleanup(ctx, 1)
		requireReceiptCondition(t, n == 0, n, err)
		requireReceiptCondition(t, errors.Is(err, context.DeadlineExceeded), n, err)
	}
	<-s.cleanupSelection
}

func TestCleanupFairnessMalformedDeadlineIsReportedWithoutDeletion(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r := cleanupOriginal(t, f)
	cleanupExpireAt(t, f, time.Now().Add(-72*time.Hour), r)
	// Deliberately corrupt the scheduling invariant, not physical deletion state:
	// normal retirement always assigns a deadline in the authority transaction.
	execSQL(t, f.pool, `UPDATE fry_receipt SET cleanup_due=NULL WHERE id=$1`, r.ID.String())
	n, err := f.s.FinishCleanup(ctx, 1)
	requireReceiptCondition(t, n == 0, "malformed state was hidden as ordinary contention", n, err)
	requireReceiptCondition(t, err != nil, "malformed state was hidden as ordinary contention", n, err)
	requireReceiptCondition(
		t,
		!errors.Is(err, ErrConflict),
		"malformed state was hidden as ordinary contention",
		n,
		err,
	)
	requireReceiptCondition(t, !errors.Is(err, ErrBusy), "malformed state was hidden as ordinary contention", n, err)
	{
		n := scalar(t, f.pool, `SELECT count(*) FROM fry_header_metadata WHERE receipt=$1`, r.ID.String())
		requireReceiptCondition(t, n == 1, "malformed candidate changed metadata", n)
	}
	cleanupWantState(t, f, r, "retiring")
}

func requirePinnedCleanupDebt(t *testing.T, f *fixture, a Receipt) {
	t.Helper()
	{
		n := scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin WHERE receipt=$1`, a.ID.String())
		requireReceiptCondition(t, n == 1, "actual pin lost", n)
	}
	{
		n := scalar(t, f.pool, `SELECT count(*) FROM fry_header_metadata WHERE receipt=$1`, a.ID.String())
		requireReceiptCondition(t, n == 0, "blocked pin retained metadata", n)
	}
}
