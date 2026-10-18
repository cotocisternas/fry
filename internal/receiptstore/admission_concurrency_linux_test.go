//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type admissionResult struct {
	receipt Receipt
	err     error
}

func admissionWaiting(t *testing.T, f *fixture) int64 {
	t.Helper()
	return scalar(
		t,
		f.pool,
		`SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND `+
			`wait_event_type='Lock' AND position('pg_advisory_xact_lock' in query)>0`,
		f.pool.Config().ConnConfig.RuntimeParams["application_name"],
	)
}

func admissionWait(t *testing.T, f *fixture) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for admissionWaiting(t, f) == 0 {
		requireReceiptCondition(t, !time.Now().After(deadline), "exclusive reservation did not reach advisory wait")
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOrdinaryAdmissionSharesExistingAndMissingInboxGate(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing inbox", true: "existing inbox"}[existing], func(t *testing.T) {
			testOrdinarySharedAdmission(t, existing)
		})
	}
}

func TestOrdinaryAdmissionCoexistsWhileMigrationWaitsForAllHolders(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env := f.envelope(t)
	inbox := env.Destinations[0].Inbox
	first, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer func() {
		if err := first.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}()
	second, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer func() {
		if err := second.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}()
	err = guardMigrationImport(ctx, first, newID(t), env)
	requireReceiptNoError(t, err)
	{
		err = guardMigrationImport(ctx, second, newID(t), env)
		requireReceiptCondition(t, err == nil, "ordinary guard conflicts with ordinary guard", err)
	}
	job := newID(t)
	reserved := make(chan error, 1)
	go func() {
		reserved <- f.s.CreateMigration(ctx, f.org, job, inbox, FingerprintMigrationSource("source:993", "user", false))
	}()
	admissionWait(t, f)
	err = first.Commit(ctx)
	requireReceiptNoError(t, err)
	// The second actual normal transaction still holds its shared gate.
	requireReceiptCondition(
		t,
		admissionWaiting(t, f) == 1,
		"exclusive migration did not wait for every shared admission",
	)
	select {
	case err := <-reserved:
		t.Fatal("reservation completed while ordinary admission active", err)
	default:
	}
	err = second.Commit(ctx)
	requireReceiptNoError(t, err)
	select {
	case err := <-reserved:
		requireReceiptNoError(t, err)
	case <-ctx.Done():
		t.Fatal("exclusive reservation did not recover")
	}
	{
		_, err = f.s.Accept(ctx, newID(t), env, io.NopCloser(strings.NewReader("blocked by committed migration")))
		requireReceiptCondition(t, errors.Is(err, ErrBusy), "migration winner did not fence normal ingestion", err)
	}
	{
		n := scalar(t, f.pool, `SELECT count(*) FROM fry_receipt`)
		requireReceiptCondition(t, n == 0, "staged inbox received ordinary receipt", n)
	}
}

func TestOrdinaryAdmissionStillWaitsForExclusiveReservationGate(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env := f.envelope(t)
	inbox := env.Destinations[0].Inbox
	holder, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer func() {
		if err := holder.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}()
	err = lockInboxAdmission(ctx, holder, inbox)
	requireReceiptNoError(t, err)
	done := make(chan admissionResult, 1)
	go func() {
		r, err := f.s.Accept(ctx, newID(t), env, io.NopCloser(strings.NewReader("wait for exclusive gate")))
		done <- admissionResult{r, err}
	}()
	admissionWait(t, f)
	{
		n := scalar(t, f.pool, `SELECT count(*) FROM fry_receipt`)
		requireReceiptCondition(t, n == 0, "ordinary receipt crossed exclusive reservation", n)
	}
	err = holder.Rollback(ctx)
	requireReceiptNoError(t, err)
	select {
	case result := <-done:
		requireReceiptCondition(t, result.err == nil, result.err)
	case <-ctx.Done():
		t.Fatal("ordinary ingestion did not recover after exclusive gate")
	}
}

func TestOrdinaryPreparedUploadCannotFinalizeAcrossMigrationReservation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	env := f.envelope(t)
	inbox := env.Destinations[0].Inbox
	attempt := newID(t)
	prepared := prepare(t, f, attempt, env)
	requireReceiptNoError(
		t,
		f.s.CreateMigration(ctx, f.org, newID(t), inbox, FingerprintMigrationSource("source:993", "user", false)),
	)
	copies(t, f, prepared, "delayed ordinary bytes still need final authority")
	{
		_, err := f.s.db.finalize(ctx, prepared)
		requireReceiptCondition(t, errors.Is(err, ErrBusy), "delayed finalize crossed staged inbox", err)
	}
	for _, query := range []string{
		`SELECT count(*) FROM fry_receipt`,
		`SELECT count(*) FROM fry_destination`,
		`SELECT count(*) FROM fry_work`,
	} {
		{
			n := scalar(t, f.pool, query)
			requireReceiptCondition(t, n == 0, "delayed ordinary publication escaped exclusion", n)
		}
	}
	{
		n := scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE attempt=$1 AND locator IS NOT NULL`,
			attempt.String(),
		)
		requireReceiptCondition(t, n == 2, "test did not copy before final guard", n)
	}
	requireReceiptNoError(t, f.s.Abort(ctx, f.org, attempt))
}

func TestOrdinaryAdmissionNormalizedMultipleInboxOrder(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := newID(t), newID(t)
	destinations := []Destination{
		{Inbox: a, Recipients: []string{"a@example.test"}},
		{Inbox: b, Recipients: []string{"b@example.test"}},
	}
	holder, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer func() {
		if err := holder.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}()
	for _, inbox := range []ID{a, b} {
		err = lockOrdinaryInboxAdmission(ctx, holder, inbox)
		requireReceiptNoError(t, err)
	}
	results := make(chan error, 2)
	for _, order := range [][]Destination{destinations, {destinations[1], destinations[0]}} {
		env := Envelope{Organization: f.org, MailFrom: "sender@example.test", Destinations: order}
		attempt := newID(t)
		go func() {
			_, err := f.s.Accept(ctx, attempt, env, io.NopCloser(strings.NewReader("two normalized destinations")))
			results <- err
		}()
	}
	for range 2 {
		select {
		case err := <-results:
			requireReceiptNoError(t, err)
		case <-ctx.Done():
			t.Fatal("multi-inbox ordinary ingestion did not progress")
		}
	}
	{
		n := scalar(t, f.pool, `SELECT count(*) FROM fry_destination`)
		requireReceiptCondition(t, n == 4, "multi-inbox destinations changed", n)
	}
	{
		n := scalar(t, f.pool, `SELECT count(*) FROM fry_work`)
		requireReceiptCondition(t, n == 4, "multi-inbox work changed", n)
	}
}

func testOrdinarySharedAdmission(t *testing.T, existing bool) {
	t.Helper()

	f := setup(t)
	ctx := context.Background()
	env := f.envelope(t)
	inbox := env.Destinations[0].Inbox
	if existing {
		requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	}
	holder, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer func() {
		if err := holder.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}()
	_, err = holder.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock_shared(hashtextextended('fry-inbox-admission:' || $1,0))`,
		inbox.String(),
	)
	requireReceiptNoError(t, err)
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan admissionResult, 1)
	attempt := newID(t)
	go func() {
		receipt, err := f.s.Accept(
			bounded,
			attempt,
			env,
			io.NopCloser(strings.NewReader("ordinary ingestion shares gate")),
		)
		done <- admissionResult{receipt, err}
	}()
	for {
		select {
		case result := <-done:
			requireReceiptCondition(t, result.err == nil, "shared ordinary admission failed", result.err)
			status, err := f.s.Status(ctx, f.org, attempt)
			requireReceiptCondition(t, err == nil, status, err)
			requireReceiptCondition(t, status.State == "accepted", status, err)
			requireReceiptCondition(t, status.Receipt.ID == result.receipt.ID, status, err)
			requireReceiptNoError(t, holder.Rollback(ctx))
			requireAcceptedOrdinaryDestinationRejectsMigration(t, bounded, f, inbox)
			return
		case <-bounded.Done():
			t.Fatal("ordinary admission did not complete while shared gate held")
		default:
		}
		if admissionWaiting(t, f) > 0 {
			cancel()
			if drainErr := <-done; drainErr.err != nil {
				t.Error(drainErr.err)
			}
			t.Fatal("ordinary ingestion requested exclusive admission behind compatible shared holder")
		}
		time.Sleep(time.Millisecond)
	}
}

func requireAcceptedOrdinaryDestinationRejectsMigration(t *testing.T, bounded context.Context, f *fixture, inbox ID) {
	t.Helper()
	{
		err := f.s.CreateMigration(
			bounded,
			f.org,
			newID(t),
			inbox,
			FingerprintMigrationSource("source:993", "user", false),
		)
		requireReceiptCondition(
			t,
			errors.Is(err, ErrConflict),
			"accepted ordinary destination allowed reservation",
			err,
		)
	}
}
