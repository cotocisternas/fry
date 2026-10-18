//go:build linux

package receiptstore

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func usageWant(t *testing.T, f *fixture, org ID, count, size int64) {
	t.Helper()
	got, err := f.s.OrganizationStorageUsage(context.Background(), org)
	requireReceiptCondition(t, err == nil, "logical retained storage", got, err, "want", count, size)
	requireReceiptCondition(t, got.Organization == org, "logical retained storage", got, err, "want", count, size)
	requireReceiptCondition(
		t,
		got.RetainedOriginals == count,
		"logical retained storage",
		got,
		err,
		"want",
		count,
		size,
	)
	requireReceiptCondition(t, got.RetainedBytes == size, "logical retained storage", got, err, "want", count, size)
	// Independent full-history reconciliation is test-only, never a request path.
	var actualCount, actualBytes int64
	{
		err = f.pool.QueryRow(
			context.Background(),
			`SELECT count(*),COALESCE(sum(raw_size),0)::bigint FROM fry_receipt WHERE `+
				`organization=$1 AND lifecycle!='removed'`,
			org.String(),
		).
			Scan(&actualCount, &actualBytes)
		requireReceiptCondition(t, err == nil, "receipt reconciliation", actualCount, actualBytes, err)
		requireReceiptCondition(t, actualCount == count, "receipt reconciliation", actualCount, actualBytes, err)
		requireReceiptCondition(t, actualBytes == size, "receipt reconciliation", actualCount, actualBytes, err)
	}
}

func TestStorageUsageHashDefinition(t *testing.T) {
	for raw, want := range map[string]int16{
		"00000000000000000000000000000000": 26,
		"00000000000000000000000000000001": 17,
		"ffffffffffffffffffffffffffffffff": 1,
		"0123456789abcdef0123456789abcdef": 19,
	} {
		b, receiptErr1 := hex.DecodeString(raw)
		requireReceiptNoError(t, receiptErr1)
		var receipt ID
		copy(receipt[:], b)
		{
			got := storageUsageSlot(receipt)
			requireReceiptCondition(t, got == want, raw, got, want)
		}
	}
}

func TestStorageUsageIngressIdentityAndReferenceCopies(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	other := newID(t)
	usageWant(t, f, f.org, 0, 0)
	usageWant(t, f, other, 0, 0)
	{
		_, err := f.s.OrganizationStorageUsage(ctx, ID{})
		requireReceiptCondition(t, errors.Is(err, ErrConflict), err)
	}
	env := f.envelope(t)
	inbox := env.Destinations[0].Inbox
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	// Alias normalization and a second destination still retain one original.
	env.Destinations = append(
		env.Destinations,
		env.Destinations[0],
		Destination{Inbox: newID(t), Recipients: []string{"other@example.test"}},
	)
	r := accept(t, f.s, newID(t), env, "one shared original")
	{
		_, err := f.s.PublishMailboxes(ctx, 10)
		requireReceiptCondition(t, err == nil, err)
	}
	requireReceiptNoError(t, f.s.CreateFolder(ctx, f.org, inbox, "Copies"))
	{
		_, err := f.s.CopyMail(ctx, f.org, inbox, "INBOX", "Copies", []uint32{1}, false)
		requireReceiptCondition(t, err == nil, err)
	}
	usageWant(t, f, f.org, 1, r.Size)
	total := acceptStorageUsageIngressKinds(t, f, inbox, r)
	usageWant(t, f, f.org, 5, total)
	otherEnv := outbound(t, f, "elsewhere@example.test")
	otherEnv.Organization = other
	otherReceipt := accept(t, f.s, newID(t), otherEnv, "")
	usageWant(t, f, other, 1, otherReceipt.Size)
	usageWant(t, f, f.org, 5, total)
	pending := prepare(t, f, newID(t), f.envelope(t))
	copies(t, f, pending, "unaccepted orphan bytes")
	err := f.s.Abort(ctx, f.org, pending.Attempt)
	requireReceiptNoError(t, err)
	usageWant(t, f, f.org, 5, total)
}

func TestStorageUsageRollbackLostReplyAndAcceptedReplay(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	env := f.envelope(t)
	execSQL(
		t,
		f.pool,
		`CREATE FUNCTION fail_accounted_accept() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN `+
			`RAISE EXCEPTION 'synthetic deferred receipt failure'; END $$; CREATE CONSTRAINT `+
			`TRIGGER fail_accounted_accept AFTER INSERT ON fry_receipt DEFERRABLE INITIALLY `+
			`DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_accounted_accept()`,
	)
	{
		_, err := f.s.Accept(ctx, newID(t), env, io.NopCloser(strings.NewReader("rollback")))
		requireReceiptCondition(t, errors.Is(err, ErrUncertain), err)
	}
	usageWant(t, f, f.org, 0, 0)
	execSQL(t, f.pool, `DROP TRIGGER fail_accounted_accept ON fry_receipt; DROP FUNCTION `+
		`fail_accounted_accept()`)
	attempt := newID(t)
	f.s.db.afterCommit = func() error { return io.ErrUnexpectedEOF }
	{
		_, err := f.s.Accept(ctx, attempt, env, io.NopCloser(strings.NewReader("committed")))
		requireReceiptCondition(t, errors.Is(err, ErrUncertain), err)
	}
	f.s.db.afterCommit = nil
	usageWant(t, f, f.org, 1, 9)
	replay := accept(t, f.s, attempt, env, "not the source")
	requireReceiptCondition(t, replay.Size == 9, replay)
	usageWant(t, f, f.org, 1, 9)
}

func TestStorageUsageActualPinAndCleanupCommitFailure(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r := cleanupOriginal(t, f)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.s.beforeReadGates = func() { once.Do(func() { close(entered) }); <-release }
	releaseRead := sync.OnceFunc(func() { close(release) })
	defer releaseRead()
	handle, err := f.s.Open(ctx, f.org, r.ID, 0, r.Size)
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
		t.Fatal("pin barrier")
	}
	cleanupExpireAt(t, f, time.Now().Add(-72*time.Hour), r)
	cleanupReclaim(t, f)
	{
		n, err := f.s.FinishCleanup(ctx, 10)
		requireReceiptCondition(t, n == 0, n, err)
		requireReceiptCondition(t, err == nil, n, err)
	}
	usageWant(t, f, f.org, 1, r.Size) // retiring and actually pinned still counts.
	releaseRead()
	if err := handle.Close(); err != nil {
		t.Error(err)
	}
	cleanupReclaim(t, f)
	requireStorageRemovalCommitAtomicity(t, f, r)
}

func TestStorageUsageMissingStripeFailsRemovalWithoutRepair(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r := cleanupOriginal(t, f)
	cleanupExpireAt(t, f, time.Now().Add(-72*time.Hour), r)
	cleanupReclaim(t, f)
	// Deliberate accounting corruption must be visible, not permission to silently
	// erase its authoritative receipt or synthesize an arbitrary replacement sum.
	execSQL(t, f.pool, `DELETE FROM fry_storage_usage WHERE organization=$1`, f.org.String())
	{
		n, err := f.s.FinishCleanup(ctx, 10)
		requireReceiptCondition(t, n == 0, n, err)
		requireReceiptCondition(t, err != nil, n, err)
		requireReceiptCondition(t, !errors.Is(err, ErrConflict), n, err)
	}
	cleanupWantState(t, f, r, "retiring")
}

func TestStorageUsageSameStripeConcurrencyAndTransactionReader(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pair := prepareSameStorageStripeClaims(t, f)
	for _, c := range pair {
		copies(t, f, c, "same stripe")
	}
	// Both finalizations must queue behind one actual shared stripe, without a
	// receipt/folder lock being acquired after that stripe becomes available.
	slot := storageUsageSlot(pair[0].Receipt)
	execSQL(t, f.pool, `INSERT INTO fry_storage_usage VALUES($1,$2,0,0)`, f.org.String(), slot)
	holder, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer rollback(context.Background(), holder)
	_, err = holder.Exec(
		ctx,
		`SELECT 1 FROM fry_storage_usage WHERE organization=$1 AND slot=$2 FOR UPDATE`,
		f.org.String(),
		slot,
	)
	requireReceiptNoError(t, err)
	done := make(chan error, 2)
	for _, c := range pair {
		go func(c claim) { _, err := f.s.db.finalize(ctx, c); done <- err }(c)
	}
	waitStorageStripeFinalizers(t, ctx, f)
	usageWant(t, f, f.org, 0, 0)
	err = holder.Rollback(ctx)
	requireReceiptNoError(t, err)
	for range 2 {
		err = <-done
		requireReceiptNoError(t, err)
	}
	usageWant(t, f, f.org, 2, 22)
	requireStorageUsageTransactionReader(t, ctx, f)
}

func TestStorageUsageStartupVersionAndReaderOverflow(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	org := newID(t)
	execSQL(t, f.pool, `INSERT INTO fry_storage_usage VALUES($1,0,1,9223372036854775807),($1,1,1,1)`, org.String())
	{
		_, err := f.s.OrganizationStorageUsage(ctx, org)
		requireReceiptCondition(t, err != nil, "overflow silently wrapped")
	}
	execSQL(t, f.pool, `DELETE FROM fry_storage_accounting`)
	{
		_, err := New(ctx, f.pool, f.members, f.s.db.opts)
		requireReceiptCondition(t, err != nil, "uninitialized accounting accepted")
	}
}

func TestStorageUsageStripeWaitCannotExtendAttemptDeadline(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	c := prepare(t, f, newID(t), f.envelope(t))
	copies(t, f, c, "deadline")
	slot := storageUsageSlot(c.Receipt)
	execSQL(t, f.pool, `INSERT INTO fry_storage_usage VALUES($1,$2,0,0)`, f.org.String(), slot)
	holder, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer rollback(context.Background(), holder)
	_, err = holder.Exec(
		ctx,
		`SELECT 1 FROM fry_storage_usage WHERE organization=$1 AND slot=$2 FOR UPDATE`,
		f.org.String(),
		slot,
	)
	requireReceiptNoError(t, err)
	execSQL(
		t,
		f.pool,
		`UPDATE fry_attempt SET lease_until=clock_timestamp()+interval '5 seconds' WHERE id=$1`,
		c.Attempt.String(),
	)
	done := make(chan error, 1)
	go func() { _, err := f.s.db.finalize(ctx, c); done <- err }()
	cleanupWaitLock(t, f)
	for {
		var expired bool
		err = f.pool.QueryRow(
			ctx,
			`SELECT lease_until<=clock_timestamp() FROM fry_attempt WHERE id=$1`,
			c.Attempt.String(),
		).
			Scan(&expired)
		requireReceiptNoError(t, err)
		if expired {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	err = holder.Rollback(ctx)
	requireReceiptNoError(t, err)
	{
		err = <-done
		requireReceiptCondition(t, errors.Is(err, ErrStale), "stripe wait extended lease", err)
	}
	usageWant(t, f, f.org, 0, 0)
}

func TestStorageUsageConcurrentAppendExpungeAndCrossOrganization(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	inbox := newID(t)
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	folder, err := f.s.Folder(ctx, f.org, inbox, "INBOX")
	requireReceiptNoError(t, err)
	env := Envelope{
		Organization:  f.org,
		MailboxAppend: true,
		Destinations:  []Destination{{Inbox: inbox, Folder: folder.ID, Flags: []string{"\\Deleted"}}},
	}
	for range 4 {
		accept(t, f.s, newID(t), env, "append")
	}
	other := newID(t)
	otherEnv := outbound(t, f, "other@example.test")
	otherEnv.Organization = other
	runConcurrentStorageUsageMutations(t, ctx, f, inbox, env, otherEnv)
	usageWant(t, f, f.org, 12, 72)
	usageWant(t, f, other, 8, 40)
}

func TestStorageUsageOneSnapshotVisibility(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	requireReceiptNoError(t, err)
	defer rollback(context.Background(), tx)
	before, err := ReadOrganizationStorageUsage(ctx, tx, f.org)
	requireReceiptCondition(t, err == nil, before, err)
	requireReceiptCondition(t, before.RetainedOriginals == 0, before, err)
	r := accept(t, f.s, newID(t), f.envelope(t), "atomic original")
	old, err := ReadOrganizationStorageUsage(ctx, tx, f.org)
	requireReceiptCondition(t, err == nil, "counter escaped caller snapshot", old, err)
	requireReceiptCondition(t, old == before, "counter escaped caller snapshot", old, err)
	usageWant(t, f, f.org, 1, r.Size)
}

func TestStorageUsageStripeWaitCannotExtendMigrationDeadline(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	job, _ := migrationJob(t, f)
	inventory := []MigrationFolder{{Source: "INBOX", Destination: "INBOX", UIDValidity: 1, UIDNext: 2}}
	pass := migrationBegin(t, f, job, inventory, true)
	window, err := pass.NextWindow(ctx, 1)
	requireReceiptNoError(t, err)
	observed, err := pass.Observe(ctx, *window, MigrationMessage{UID: 1, Size: 8, InternalDate: time.Now()})
	requireReceiptNoError(t, err)
	c := prepare(t, f, observed.Attempt, observed.Envelope)
	copies(t, f, c, "deadline")
	slot := storageUsageSlot(c.Receipt)
	execSQL(t, f.pool, `INSERT INTO fry_storage_usage VALUES($1,$2,0,0)`, f.org.String(), slot)
	holder, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer rollback(context.Background(), holder)
	_, err = holder.Exec(
		ctx,
		`SELECT 1 FROM fry_storage_usage WHERE organization=$1 AND slot=$2 FOR UPDATE`,
		f.org.String(),
		slot,
	)
	requireReceiptNoError(t, err)
	execSQL(
		t,
		f.pool,
		`UPDATE fry_migration SET deadline=clock_timestamp()+interval '5 seconds' WHERE id=$1`,
		job.String(),
	)
	done := make(chan error, 1)
	go func() { _, err := f.s.db.finalize(ctx, c); done <- err }()
	cleanupWaitLock(t, f)
	for {
		var expired bool
		err = f.pool.QueryRow(ctx, `SELECT deadline<=clock_timestamp() FROM fry_migration WHERE id=$1`, job.String()).
			Scan(&expired)
		requireReceiptNoError(t, err)
		if expired {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	err = holder.Rollback(ctx)
	requireReceiptNoError(t, err)
	{
		err = <-done
		requireReceiptCondition(t, errors.Is(err, ErrStale), "stripe wait extended final migration deadline", err)
	}
	usageWant(t, f, f.org, 0, 0)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_migration_copy WHERE receipt IS NOT NULL`) == 0,
		"expired final import escaped rollback",
	)
}

func acceptStorageUsageIngressKinds(t *testing.T, f *fixture, inbox ID, r Receipt) int64 {
	t.Helper()
	ctx := context.Background()
	folder, err := f.s.Folder(ctx, f.org, inbox, "INBOX")
	requireReceiptNoError(t, err)
	appended := accept(
		t,
		f.s,
		newID(t),
		Envelope{
			Organization:  f.org,
			MailboxAppend: true,
			Destinations:  []Destination{{Inbox: inbox, Folder: folder.ID, Flags: []string{"\\Seen"}}},
		},
		"inline APPEND",
	)
	outgoing := accept(t, f.s, newID(t), outbound(t, f, "recipient@example.test"), "queued outbound")
	collectedEnv := f.envelope(t)
	collectedEnv.Source = &Source{Connection: newID(t), Folder: "INBOX", UIDValidity: 1, UID: 1}
	collected := accept(t, f.s, newID(t), collectedEnv, "collected original")
	job, _ := migrationJob(t, f)
	inventory := []MigrationFolder{{Source: "INBOX", Destination: "INBOX", UIDValidity: 1, UIDNext: 2}}
	pass := migrationBegin(t, f, job, inventory, false)
	history := migrationScan(
		t,
		f,
		pass,
		map[string]map[uint32]MigrationMessage{"INBOX": {1: {UID: 1, Size: 7, InternalDate: time.Now()}}},
		map[string]string{"INBOX": "history"},
	)["INBOX"]
	total := r.Size + appended.Size + outgoing.Size + collected.Size + history.Size
	return total
}

func requireStorageRemovalCommitAtomicity(t *testing.T, f *fixture, r Receipt) {
	t.Helper()
	ctx := context.Background()
	execSQL(
		t,
		f.pool,
		`CREATE FUNCTION fail_accounted_removal() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN `+
			`IF NEW.lifecycle='removed' THEN RAISE EXCEPTION 'synthetic removal failure'; END IF; `+
			`RETURN NULL; END $$; CREATE CONSTRAINT TRIGGER fail_accounted_removal AFTER UPDATE ON `+
			`fry_receipt DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION `+
			`fail_accounted_removal()`,
	)
	{
		n, err := f.s.FinishCleanup(ctx, 10)
		requireReceiptCondition(t, n == 0, n, err)
		requireReceiptCondition(t, errors.Is(err, ErrUncertain), n, err)
	}
	usageWant(t, f, f.org, 1, r.Size)
	cleanupWantState(t, f, r, "retiring")
	execSQL(t, f.pool, `DROP TRIGGER fail_accounted_removal ON fry_receipt; DROP FUNCTION `+
		`fail_accounted_removal()`)
	{
		n, err := f.s.FinishCleanup(ctx, 10)
		requireReceiptCondition(t, n == 1, n, err)
		requireReceiptCondition(t, err == nil, n, err)
	}
	usageWant(t, f, f.org, 0, 0)
	{
		n, err := f.s.FinishCleanup(ctx, 10)
		requireReceiptCondition(t, n == 0, n, err)
		requireReceiptCondition(t, err == nil, n, err)
	}
	usageWant(t, f, f.org, 0, 0)
}

func prepareSameStorageStripeClaims(t *testing.T, f *fixture) []claim {
	t.Helper()
	buckets := map[int16]claim{}
	var pair []claim
	for range 65 {
		c := prepare(t, f, newID(t), f.envelope(t))
		slot := storageUsageSlot(c.Receipt)
		if previous, ok := buckets[slot]; ok {
			pair = []claim{previous, c}
			break
		}
		buckets[slot] = c
	}
	requireReceiptCondition(t, len(pair) == 2, "pigeonhole bound")
	return pair
}

func waitStorageStripeFinalizers(t *testing.T, ctx context.Context, f *fixture) {
	t.Helper()
	var err error
	for {
		var blocked int
		err = f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND `+
			`wait_event_type='Lock'`, f.pool.Config().ConnConfig.RuntimeParams["application_name"]).
			Scan(&blocked)
		requireReceiptNoError(t, err)
		if blocked == 2 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("two stripe waiters", ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func requireStorageUsageTransactionReader(t *testing.T, ctx context.Context, f *fixture) {
	t.Helper()
	cfg := f.pool.Config().Copy()
	cfg.MaxConns = 1
	cfg.MinConns = 0
	single, err := pgxpool.NewWithConfig(ctx, cfg)
	requireReceiptNoError(t, err)
	defer single.Close()
	tx, err := single.Begin(ctx)
	requireReceiptNoError(t, err)
	defer rollback(context.WithoutCancel(ctx), tx)
	got, err := ReadOrganizationStorageUsage(ctx, tx, f.org)
	requireReceiptCondition(t, err == nil, "same authorized connection reader", got, err)
	requireReceiptCondition(t, got.RetainedOriginals == 2, "same authorized connection reader", got, err)
	requireReceiptCondition(t, got.RetainedBytes == 22, "same authorized connection reader", got, err)
}

func runConcurrentStorageUsageMutations(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	inbox ID,
	env, otherEnv Envelope,
) {
	t.Helper()
	var err error
	done := make(chan error, 3)
	go appendStorageUsageOriginals(t, ctx, f, env, "append", done)
	go func() {
		for range 8 {
			_, err := f.s.Expunge(ctx, f.org, inbox, "INBOX", nil)
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	go appendStorageUsageOriginals(t, ctx, f, otherEnv, "other", done)
	for range 3 {
		err = <-done
		requireReceiptNoError(t, err)
	}
}

func appendStorageUsageOriginals(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	env Envelope,
	body string,
	done chan<- error,
) {
	t.Helper()

	for range 8 {
		_, err := f.s.Accept(ctx, newID(t), env, io.NopCloser(strings.NewReader(body)))
		if err != nil {
			done <- err
			return
		}
	}
	done <- nil
}
