//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func submissionSeed(t *testing.T, f *fixture) {
	t.Helper()
	_, err := f.pool.Exec(
		context.Background(),
		`INSERT INTO `+
			`fry_submission_account(username,organization,password_hash,enabled,`+
			`authorized_senders,authorized_domains) `+
			`VALUES('robot',$1,'synthetic',true,'{}',ARRAY['example.test'])`,
		f.org.String(),
	)
	requireReceiptNoError(t, err)
}

func submissionDisable(t *testing.T, f *fixture) {
	t.Helper()
	_, err := f.pool.Exec(
		context.Background(),
		`UPDATE fry_submission_account SET enabled=false,version=version+1 WHERE `+
			`username='robot'`,
	)
	requireReceiptNoError(t, err)
}

func TestSubmissionProofPrepareFinalizeReplay(t *testing.T) {
	f := setup(t)
	submissionSeed(t, f)
	ctx := context.Background()
	env := outbound(t, f, "recipient@example.test")
	env.Outbound.SubmissionAuthorization = &SubmissionAuthorization{Username: "robot", Version: 1}
	attempt := newID(t)
	r := accept(t, f.s, attempt, env, "accepted")
	submissionDisable(t, f)
	if _, err := f.s.Accept(
		ctx,
		newID(t),
		env,
		io.NopCloser(strings.NewReader("stale")),
	); !errors.Is(
		err,
		ErrSubmissionUnauthorized,
	) {
		t.Fatal(err)
	}
	replay := accept(t, f.s, attempt, env, "unchanged")
	requireReceiptCondition(t, replay.ID == r.ID, "replay changed")
	if _, err := f.s.ClaimOutbound(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE fry_submission_account SET enabled=true,version=version+1`); err != nil {
		t.Fatal(err)
	}
	env.Outbound.SubmissionAuthorization.Version = 3
	f.s.afterCopies = func() error { submissionDisable(t, f); return nil }
	if _, err := f.s.Accept(
		ctx,
		newID(t),
		env,
		io.NopCloser(strings.NewReader("disabled during copy")),
	); !errors.Is(
		err,
		ErrSubmissionUnauthorized,
	) {
		t.Fatal(err)
	}
	if n := scalar(t, f.pool, `SELECT count(*) FROM fry_receipt`); n != 1 {
		t.Fatal(n)
	}
}

func TestSubmissionProofCommitPrecedesDisable(t *testing.T) {
	f := setup(t)
	submissionSeed(t, f)
	ctx := context.Background()
	env := outbound(t, f, "recipient@example.test")
	env.Outbound.SubmissionAuthorization = &SubmissionAuthorization{Username: "robot", Version: 1}
	atCommit, release := make(chan struct{}), make(chan struct{})
	f.s.db.beforeCommit = func(pgx.Tx) error { close(atCommit); <-release; return nil }
	done := make(chan error, 1)
	go func() {
		_, err := f.s.Accept(ctx, newID(t), env, io.NopCloser(strings.NewReader("commit wins")))
		done <- err
	}()
	select {
	case <-atCommit:
	case <-time.After(5 * time.Second):
		t.Fatal("no commit")
	}
	disabled := make(chan error, 1)
	go func() {
		_, err := f.pool.Exec(ctx, `UPDATE fry_submission_account SET enabled=false,version=version+1`)
		disabled <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for scalar(t, f.pool, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND `+
		`wait_event_type='Lock'`, f.pool.Config().ConnConfig.RuntimeParams["application_name"]) == 0 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("disable did not block")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	requireReceiptNoError(t, <-done)
	requireReceiptNoError(t, <-disabled)
}

func TestSubmissionProofNormalizedCopy(t *testing.T) {
	f := setup(t)
	env := outbound(t, f, "recipient@example.test")
	p := &SubmissionAuthorization{Username: "robot", Version: 1}
	env.Outbound.SubmissionAuthorization = p
	frozen, _, err := normalize(env)
	requireReceiptNoError(t, err)
	p.Version = 2
	requireReceiptCondition(t, frozen.Outbound.SubmissionAuthorization.Version == 1, "mutable proof")
}

func TestSubmissionScopeAuthorityAndFinalizationFence(t *testing.T) {
	f := setup(t)
	submissionSeed(t, f)
	ctx := context.Background()
	env := outbound(t, f, "recipient@example.test")
	env.Outbound.SubmissionAuthorization = &SubmissionAuthorization{Username: "robot", Version: 1}
	denied := env
	denied.MailFrom = "outside@unapproved.test"
	if _, err := f.s.Accept(
		ctx,
		newID(t),
		denied,
		io.NopCloser(strings.NewReader("denied")),
	); !errors.Is(
		err,
		ErrSubmissionUnauthorized,
	) {
		t.Fatal("durable guard ignored scope", err)
	}
	c := prepare(t, f, newID(t), env)
	copies(t, f, c, "copied before policy change")
	execSQL(
		t,
		f.pool,
		`UPDATE fry_submission_account SET `+
			`authorized_domains=ARRAY['new.test'],version=version+1 WHERE username='robot'`,
	)
	if _, err := f.s.db.finalize(ctx, c); !errors.Is(err, ErrSubmissionUnauthorized) {
		t.Fatal("stale policy finalized", err)
	}
	requireReceiptCondition(t, scalar(t, f.pool, `SELECT count(*) FROM fry_receipt`) == 0, "stale policy receipt")
	env.Outbound.SubmissionAuthorization.Version = 2
	if _, err := f.s.Accept(
		ctx,
		newID(t),
		env,
		io.NopCloser(strings.NewReader("new proof wrongscope")),
	); !errors.Is(
		err,
		ErrSubmissionUnauthorized,
	) {
		t.Fatal("freshversion bypassed scope", err)
	}
	env.MailFrom = "Sender@NEW.TEST"
	accept(t, f.s, newID(t), env, "new policy accepted")
}

func TestSubmissionPolicyUpdateWaitsForAcceptedCommit(t *testing.T) {
	f := setup(t)
	submissionSeed(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	env := outbound(t, f, "recipient@example.test")
	env.Outbound.SubmissionAuthorization = &SubmissionAuthorization{Username: "robot", Version: 1}
	attempt := newID(t)
	atCommit, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	f.s.db.beforeCommit = func(pgx.Tx) error {
		close(atCommit)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.s.Accept(ctx, attempt, env, io.NopCloser(strings.NewReader("accepted before scope narrowing")))
		done <- err
	}()
	select {
	case <-atCommit:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	changed := make(chan error, 1)
	go func() {
		_, err := f.pool.Exec(
			ctx,
			`UPDATE fry_submission_account SET `+
				`authorized_domains=ARRAY['new.test'],version=version+1 WHERE username='robot'`,
		)
		changed <- err
	}()
	for scalar(t, f.pool, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND `+
		`wait_event_type='Lock'`, f.pool.Config().ConnConfig.RuntimeParams["application_name"]) == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("policy update did not wait", ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	unblock()
	requireReceiptNoError(t, <-done)
	requireReceiptNoError(t, <-changed)
	f.s.db.beforeCommit = nil
	if _, err := f.s.Accept(ctx, attempt, env, io.NopCloser(strings.NewReader("accepted replay"))); err != nil {
		t.Fatal("policy edit revoked accepted receipt", err)
	}
}
