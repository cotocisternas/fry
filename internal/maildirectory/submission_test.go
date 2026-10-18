//go:build linux

package maildirectory

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/credentials"
	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func submissionDirectory(t *testing.T) (*Directory, receiptstore.ID) {
	t.Helper()
	dsn := os.Getenv("FRY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	org, err := hotstore.NewID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "fry_submission_" + org.String()
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.RuntimeParams["application_name"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err = receiptstore.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return New(db), org
}

func TestSubmissionDirectorySeedAndMutations(t *testing.T) {
	d, org := submissionDirectory(t)
	ctx := context.Background()
	a := []SubmissionAccount{
		{
			Username:          "robot",
			Organization:      org.String(),
			Enabled:           true,
			AuthorizedDomains: []string{"business.test", "example.test"},
		},
	}
	if err := d.SeedSubmission(ctx, a, []string{"first-password"}); err != nil {
		t.Fatal(err)
	}
	p, err := d.AuthenticateSubmission(ctx, "robot", "first-password")
	if err != nil {
		t.Fatal(err)
	}
	other := rotateSubmissionInTest(t, ctx, d, org)
	if err = d.ValidateSubmission(ctx, p); !errors.Is(err, ErrNotFound) {
		t.Fatal("old proof", err)
	}
	for _, secret := range []string{"", "stale-password"} {
		if err = d.SeedSubmission(ctx, a, []string{secret}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = d.AuthenticateSubmission(ctx, "robot", "first-password"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	fresh, err := d.AuthenticateSubmission(ctx, "robot", "second-password")
	if err != nil || fresh.Version != 2 {
		t.Fatal(fresh, err)
	}
	requireSeedKeepsSubmissionDisabled(t, ctx, d, org, a)
	requireSubmissionSeedFailures(t, ctx, d, a, other)
}

func requireSeedKeepsSubmissionDisabled(
	t *testing.T,
	ctx context.Context,
	d *Directory,
	org receiptstore.ID,
	a []SubmissionAccount,
) {
	t.Helper()
	tx, err := d.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTest(t, ctx, tx)
	if err = SetSubmissionAccess(ctx, tx, org, "robot", false); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = d.SeedSubmission(ctx, a, []string{"first-password"}); err != nil {
		t.Fatal(err)
	}
	if _, err = d.AuthenticateSubmission(ctx, "robot", "second-password"); !errors.Is(err, ErrNotFound) {
		t.Fatal("seed reenabled", err)
	}
}

func rotateSubmissionInTest(t *testing.T, ctx context.Context, d *Directory, org receiptstore.ID) receiptstore.ID {
	t.Helper()
	hash, err := credentials.HashPassword("second-password")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := d.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTest(t, ctx, tx)
	other, err := hotstore.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err = RotateSubmission(ctx, tx, other, "robot", hash); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-org", err)
	}
	if err = RotateSubmission(ctx, tx, org, "robot", hash); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return other
}

func TestSubmissionLiveCreationPolicyAndPermanentReservation(t *testing.T) {
	d, org := submissionDirectory(t)
	ctx := context.Background()
	encoded, err := credentials.HashPassword("created-secret")
	if err != nil {
		t.Fatal(err)
	}
	a := SubmissionAccount{
		Username:          "new-user",
		Organization:      org.String(),
		Enabled:           true,
		AuthorizedSenders: []string{"Robot@Example.TEST"},
	}
	tx, err := d.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTest(t, ctx, tx)
	if err = CreateSubmission(ctx, tx, a, encoded); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := d.AuthenticateSubmission(ctx, a.Username, "created-secret")
	if err != nil || len(p.AuthorizedSenders) != 1 || p.AuthorizedSenders[0] != "Robot@example.test" {
		t.Fatal(p, err)
	}
	disableSubmissionInTest(t, ctx, d, org, a.Username)
	if err = d.ValidateSubmission(ctx, p); !errors.Is(err, ErrNotFound) {
		t.Fatal("policy did not revoke proof", err)
	}
	// Bootstrap is insert-only even if it carries stale policy/access/password.
	if err = d.SeedSubmission(ctx, []SubmissionAccount{a}, []string{"stale-password"}); err != nil {
		t.Fatal(err)
	}
	requireSubmissionNameReserved(t, ctx, d, a, encoded)
}

func disableSubmissionInTest(t *testing.T, ctx context.Context, d *Directory, org receiptstore.ID, username string) {
	t.Helper()
	tx, err := d.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTest(t, ctx, tx)
	if err = SetSubmissionPolicy(ctx, tx, org, username, nil, []string{"New.TEST"}); err != nil {
		t.Fatal(err)
	}
	if err = SetSubmissionAccess(ctx, tx, org, username, false); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSubmissionConcurrentCreatesAndLaterSeedDoNotOverwrite(t *testing.T) {
	d, org := submissionDirectory(t)
	ctx := context.Background()
	encoded, err := credentials.HashPassword("create-secret")
	if err != nil {
		t.Fatal(err)
	}
	a := SubmissionAccount{
		Username:          "race",
		Organization:      org.String(),
		Enabled:           false,
		AuthorizedDomains: []string{"create.test"},
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- createSubmissionInTest(t, ctx, d, a, encoded)
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		err = <-results
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatal(successes)
	}
	seed := a
	seed.Enabled = true
	seed.AuthorizedDomains = []string{"seed.test"}
	if err = d.SeedSubmission(ctx, []SubmissionAccount{seed}, []string{"seed-secret"}); err != nil {
		t.Fatal(err)
	}
	requireCreatedSubmissionUnchanged(t, ctx, d)
}

func TestSubmissionScopesRejectAmbiguityAndBounds(t *testing.T) {
	for _, scope := range []string{"*", " *.test", "example.test ", "a@example.test", "bad..test"} {
		if _, _, err := receiptstore.NormalizeSubmissionScopes(nil, []string{scope}); err == nil {
			t.Fatal(scope)
		}
	}
	if _, _, err := receiptstore.NormalizeSubmissionScopes(nil, nil); err == nil {
		t.Fatal("empty allow")
	}
	if _, _, err := receiptstore.NormalizeSubmissionScopes(nil, make([]string, 129)); err == nil {
		t.Fatal("unbounded list")
	}
	if !receiptstore.SubmissionScopeAllows([]string{"Robot@Example.TEST"}, nil, "Robot@example.test") ||
		receiptstore.SubmissionScopeAllows([]string{"Robot@example.test"}, nil, "robot@example.test") {
		t.Fatal("case semantics")
	}
}

func TestSubmissionSeedConcurrentWithCreateAndOldSchemaFailsClosed(t *testing.T) {
	d, org := submissionDirectory(t)
	encoded, err := credentials.HashPassword("created-secret")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a := SubmissionAccount{
		Username:          "seed-race",
		Organization:      org.String(),
		Enabled:           true,
		AuthorizedDomains: []string{"create.test"},
	}
	seed := a
	seed.AuthorizedDomains = []string{"seed.test"}
	holder, err := d.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTest(t, ctx, holder)
	if _, err = holder.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('fry/submission/' || $1,0))`,
		a.Username,
	); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- d.SeedSubmission(ctx, []SubmissionAccount{seed}, []string{"seed-secret"}) }()
	go func() {
		<-start
		results <- createSubmissionInTest(t, ctx, d, a, encoded)
	}()
	close(start)
	waitForSubmissionWriters(t, ctx, d)
	if err = holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err = <-results; err != nil && !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	requireConsistentSubmissionWinner(t, ctx, d, a)
}

func waitForSubmissionWriters(t *testing.T, ctx context.Context, d *Directory) {
	t.Helper()
	for {
		var blocked int
		if err := d.db.QueryRow(
			ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND `+
				`wait_event_type='Lock' AND wait_event='advisory'`,
			d.db.Config().ConnConfig.RuntimeParams["application_name"],
		).
			Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked == 2 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("create/seed did not both wait on shared username key", ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func rollbackTest(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := tx.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Error(err)
	}
}

func createSubmissionInTest(
	t *testing.T,
	ctx context.Context,
	d *Directory,
	a SubmissionAccount,
	encoded string,
) error {
	t.Helper()
	tx, err := d.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackTest(t, ctx, tx)
	if err := CreateSubmission(ctx, tx, a, encoded); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func requireSubmissionSeedFailures(
	t *testing.T,
	ctx context.Context,
	d *Directory,
	a []SubmissionAccount,
	other receiptstore.ID,
) {
	t.Helper()
	a[0].Organization = other.String()
	if err := d.SeedSubmission(ctx, a, []string{""}); !errors.Is(err, ErrConflict) {
		t.Fatal("rebound", err)
	}
	a[0].Username = "missing"
	if err := d.SeedSubmission(ctx, a, []string{""}); err == nil {
		t.Fatal("missing secret allowed")
	}
	d.db.Close()
	if _, err := d.AuthenticateSubmission(ctx, "robot", "second-password"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatal("DB outage hidden", err)
	}
}

func requireSubmissionNameReserved(
	t *testing.T,
	ctx context.Context,
	d *Directory,
	a SubmissionAccount,
	encoded string,
) {
	t.Helper()
	tx, err := d.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackTest(t, ctx, tx)
	if err := CreateSubmission(ctx, tx, a, encoded); !errors.Is(err, ErrConflict) {
		t.Fatal("disabled name reused", err)
	}
	rollbackTest(t, ctx, tx)
	var enabled bool
	var version int64
	var domains []string
	if err := d.db.QueryRow(
		ctx,
		`SELECT enabled,version,authorized_domains FROM fry_submission_account WHERE username=$1`,
		a.Username,
	).
		Scan(&enabled, &version, &domains); err != nil || enabled || version != 3 ||
		len(domains) != 1 ||
		domains[0] != "new.test" {
		t.Fatal(enabled, version, domains, err)
	}
}

func requireCreatedSubmissionUnchanged(t *testing.T, ctx context.Context, d *Directory) {
	t.Helper()
	var enabled bool
	var domains []string
	var hash string
	if err := d.db.QueryRow(ctx, `SELECT enabled,authorized_domains,password_hash FROM fry_submission_account `+
		`WHERE username='race'`).
		Scan(&enabled, &domains, &hash); err != nil ||
		enabled ||
		domains[0] != "create.test" ||
		!credentials.VerifyHash(hash, "create-secret") {
		t.Fatal(enabled, domains, err)
	}
}

func requireConsistentSubmissionWinner(t *testing.T, ctx context.Context, d *Directory, a SubmissionAccount) {
	t.Helper()
	var hash string
	var domains []string
	if err := d.db.QueryRow(
		ctx,
		`SELECT password_hash,authorized_domains FROM fry_submission_account WHERE username=$1`,
		a.Username,
	).
		Scan(&hash, &domains); err != nil {
		t.Fatal(err)
	}
	seedPair := domains[0] == "seed.test" && credentials.VerifyHash(hash, "seed-secret")
	createdPair := domains[0] == "create.test" && credentials.VerifyHash(hash, "created-secret")
	if !seedPair && !createdPair {
		t.Fatal("mixed policy/secret", domains)
	}
	// An older schema is not implicitly backfilled, including an empty bootstrap.
	if _, err := d.db.Exec(
		ctx,
		`ALTER TABLE fry_submission_account DROP COLUMN authorized_senders CASCADE`,
	); err != nil {
		t.Fatal(err)
	}
	if err := d.SeedSubmission(ctx, nil, nil); err == nil {
		t.Fatal("old schema allowed empty bootstrap")
	}
}
