//go:build linux

package receiptstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func senderTx(t *testing.T, f *fixture, operation func(pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}()
	if err = operation(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func senderOverride(t *testing.T, f *fixture, scope string) SenderGrant {
	t.Helper()
	var grant SenderGrant
	err := senderTx(t, f, func(tx pgx.Tx) error {
		var err error
		grant, err = OverrideSenderGrant(
			context.Background(),
			tx,
			f.org,
			scope,
			"global-test-operator",
			"documented delegation override",
		)
		return err
	})
	requireReceiptNoError(t, err)
	return grant
}

func senderChallenge(t *testing.T, f *fixture, scope string) SenderChallenge {
	t.Helper()
	var challenge SenderChallenge
	err := senderTx(t, f, func(tx pgx.Tx) error {
		var err error
		challenge, err = CreateSenderChallenge(context.Background(), tx, f.org, scope)
		return err
	})
	requireReceiptNoError(t, err)
	return challenge
}

func senderComplete(t *testing.T, f *fixture, challenge SenderChallenge, txt []string) (SenderGrant, error) {
	t.Helper()
	var grant SenderGrant
	err := senderTx(t, f, func(tx pgx.Tx) error {
		var err error
		grant, err = CompleteSenderChallenge(context.Background(), tx, f.org, challenge.ID, txt, "assigned-manager")
		return err
	})
	return grant, err
}

func senderRevoke(t *testing.T, f *fixture, scope string) {
	t.Helper()
	requireReceiptNoError(t, senderTx(t, f, func(tx pgx.Tx) error {
		return RevokeSenderGrant(context.Background(), tx, f.org, scope, "assigned-manager", "delegation revoked")
	}))
}

func TestSenderScopeCanonicalizationAndDNSNameBounds(t *testing.T) {
	for raw, want := range map[string]string{
		"EXAMPLE.Test":             "example.test",
		"Exact.Local@EXAMPLE.Test": "Exact.Local@example.test",
	} {
		{
			got, err := CanonicalSenderScope(raw)
			requireReceiptCondition(t, err == nil, got, want, err)
			requireReceiptCondition(t, got == want, got, want, err)
		}
	}
	for _, scope := range []string{
		"",
		" example.test",
		"*.example.test",
		"a..test",
		"-bad.test",
		"Name <a@example.test>",
		"a@@example.test",
		"á@example.test",
		"a@example.test.",
	} {
		{
			_, err := CanonicalSenderScope(scope)
			requireReceiptCondition(t, err != nil, "invalid scope accepted", scope)
		}
	}
	maximum := strings.Repeat(
		"a",
		63,
	) + "." + strings.Repeat(
		"b",
		63,
	) + "." + strings.Repeat(
		"c",
		63,
	) + "." + strings.Repeat(
		"d",
		49,
	)
	requireReceiptCondition(t, len(maximum) == 241, len(maximum))
	{
		_, err := CanonicalSenderScope(maximum)
		requireReceiptCondition(t, err == nil, err)
	}
	{
		_, err := CanonicalSenderScope(maximum + "x")
		requireReceiptCondition(t, err != nil, "overlong prefixed DNS name accepted")
	}
}

func TestSenderChallengeHashScopeIsolationAndExactTXT(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	before := time.Now()
	challenge := senderChallenge(t, f, "Exact@EXAMPLE.test")
	requireReceiptCondition(t, challenge.DNSName == "_fry-verify.example.test.", challenge)
	requireReceiptCondition(t, len(challenge.TXTValue) == len("fry-sender-v1=")+64, challenge)
	requireReceiptCondition(t, !challenge.ExpiresAt.Before(before.Add(24*time.Hour-time.Second)), challenge)
	requireReceiptCondition(t, !challenge.ExpiresAt.After(time.Now().Add(24*time.Hour+time.Second)), challenge)
	{
		_, err := hex.DecodeString(strings.TrimPrefix(challenge.TXTValue, "fry-sender-v1="))
		requireReceiptCondition(t, err == nil, err)
	}
	requireSenderChallengeSecretHashed(t, ctx, f, challenge)
	requireReceiptNoError(t, senderTx(t, f, func(tx pgx.Tx) error {
		looked, err := LookupSenderChallenge(ctx, tx, f.org, challenge.ID)
		requireReceiptCondition(t, err != nil || (looked.TXTValue == "" && looked.DNSName == challenge.DNSName), looked)
		return err
	}))
	{
		err := senderTx(
			t,
			f,
			func(tx pgx.Tx) error { _, err := LookupSenderChallenge(ctx, tx, newID(t), challenge.ID); return err },
		)
		requireReceiptCondition(t, errors.Is(err, ErrNotFound), "cross-org challenge visible", err)
	}
	for _, txt := range [][]string{
		{challenge.TXTValue + " "},
		{strings.ToUpper(challenge.TXTValue)},
		{challenge.TXTValue[:25], challenge.TXTValue[25:]},
	} {
		{
			_, err := senderComplete(t, f, challenge, txt)
			requireReceiptCondition(t, errors.Is(err, ErrSenderUnauthorized), "TXT records combined/normalized", err)
		}
	}
	grant, err := senderComplete(t, f, challenge, []string{"irrelevant", challenge.TXTValue})
	requireReceiptCondition(t, err == nil, grant, err)
	requireReceiptCondition(t, grant.Active, grant, err)
	requireReceiptCondition(t, grant.Scope == "Exact@example.test", grant, err)
	requireReceiptCondition(t, grant.Actor == "assigned-manager", grant, err)
	requireReceiptCondition(t, grant.Evidence == "dns-txt", grant, err)
	_, err = senderComplete(t, f, challenge, []string{challenge.TXTValue})
	requireReceiptCondition(t, errors.Is(err, ErrSenderUnauthorized), "challenge replay accepted", err)
	_, err = f.s.AuthorizeSender(ctx, f.org, "Exact@example.test")
	requireReceiptNoError(t, err)
	{
		_, err = f.s.AuthorizeSender(ctx, f.org, "exact@example.test")
		requireReceiptCondition(t, errors.Is(err, ErrSenderUnauthorized), "exact localpart case folded", err)
	}
	encoded, err := json.Marshal(grant)
	requireReceiptCondition(t, err == nil, string(encoded), err)
	requireReceiptCondition(t, strings.Contains(string(encoded), `"id":"`+grant.ID.String()+`"`), string(encoded), err)
	requireReceiptCondition(
		t,
		strings.Contains(string(encoded), `"organization":"`+f.org.String()+`"`),
		string(encoded),
		err,
	)
}

func TestSenderChallengeSupersessionExpiryRevocationAndOverride(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	old := senderChallenge(t, f, "example.test")
	latest := senderChallenge(t, f, "EXAMPLE.test")
	{
		_, err := senderComplete(t, f, old, []string{old.TXTValue})
		requireReceiptCondition(t, errors.Is(err, ErrSenderUnauthorized), "superseded challenge activated", err)
	}
	{
		_, err := f.pool.Exec(
			ctx,
			`UPDATE fry_sender_challenge SET expires_at=clock_timestamp()-interval '1 second' `+
				`WHERE id=$1`,
			latest.ID.String(),
		)
		requireReceiptCondition(t, err == nil, err)
	}
	{
		_, err := senderComplete(t, f, latest, []string{latest.TXTValue})
		requireReceiptCondition(t, errors.Is(err, ErrSenderUnauthorized), "expired challenge activated", err)
	}
	revoked := senderChallenge(t, f, "example.test")
	senderRevoke(t, f, "example.test")
	{
		_, err := senderComplete(t, f, revoked, []string{revoked.TXTValue})
		requireReceiptCondition(t, errors.Is(err, ErrSenderUnauthorized), "revoked challenge activated", err)
	}
	pending := senderChallenge(t, f, "example.test")
	grant := senderOverride(t, f, "example.test")
	{
		_, err := senderComplete(t, f, pending, []string{pending.TXTValue})
		requireReceiptCondition(t, errors.Is(err, ErrSenderUnauthorized), "pre-override challenge activated", err)
	}
	requireReceiptCondition(t, grant.Evidence == "global-operator-override", grant)
	requireReceiptCondition(t, grant.Reason != "", grant)
	requireReceiptCondition(t, grant.VerifiedAt != nil, grant)
	{
		err := senderTx(t, f, func(tx pgx.Tx) error {
			_, err := OverrideSenderGrant(ctx, tx, f.org, "other.test", "operator", "")
			return err
		})
		requireReceiptCondition(t, errors.Is(err, ErrSenderUnauthorized), "override without reason allowed", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_sender_grant_event WHERE grant_id=$1`, grant.ID.String()) == 2,
		"revocation/override evidence missing",
	)
	requireReceiptNoError(t, senderTx(t, f, func(tx pgx.Tx) error {
		grants, err := ListSenderGrants(ctx, tx, f.org)
		requireReceiptCondition(t, err != nil || (len(grants) == 1 && grants[0].ID == grant.ID), grants)
		return err
	}))
}

func TestSenderAuthorizationOrgScopeVersionFenceAndAcceptedReplay(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	senderOverride(t, f, "example.test")
	exact := senderOverride(t, f, "sender@example.test")
	proof, err := f.s.AuthorizeSender(ctx, f.org, "sender@example.test")
	requireReceiptCondition(t, err == nil, "exact scope not preferred", proof, err)
	requireReceiptCondition(t, proof.Grant == exact.ID, "exact scope not preferred", proof, err)
	{
		_, err = f.s.AuthorizeSender(ctx, newID(t), "sender@example.test")
		requireReceiptCondition(t, errors.Is(err, ErrSenderUnauthorized), "cross-org sender grant used", err)
	}
	env := outbound(t, f, "recipient@example.test")
	env.MailFrom = "sender@example.test"
	env.Outbound.SenderAuthorization = proof
	attempt := newID(t)
	receipt := accept(t, f.s, attempt, env, "durably accepted")
	senderRevoke(t, f, "sender@example.test")
	{
		_, err = f.s.Accept(ctx, newID(t), env, io.NopCloser(strings.NewReader("stale")))
		requireReceiptCondition(t, errors.Is(err, ErrSenderUnauthorized), "stale proof accepted during prepare", err)
	}
	replay := accept(t, f.s, attempt, env, "must not replace accepted bytes")
	requireReceiptCondition(t, replay.ID == receipt.ID, "accepted replay changed after revoke", replay)
	{
		claim, err := f.s.ClaimOutbound(ctx, time.Minute)
		requireReceiptCondition(t, err == nil, "revocation removed already accepted queue", claim, err)
		requireReceiptCondition(t, claim.Receipt == receipt.ID, "revocation removed already accepted queue", claim, err)
	}
	senderOverride(t, f, "sender@example.test")
	fresh, err := f.s.AuthorizeSender(ctx, f.org, "sender@example.test")
	requireReceiptCondition(t, err == nil, fresh, err)
	requireReceiptCondition(t, fresh.Version > proof.Version, fresh, err)
	env.Outbound.SenderAuthorization = fresh
	f.s.afterCopies = func() error { senderRevoke(t, f, "sender@example.test"); return nil }
	defer func() { f.s.afterCopies = nil }()
	{
		_, err = f.s.Accept(ctx, newID(t), env, io.NopCloser(strings.NewReader("revoked while copying")))
		requireReceiptCondition(t, errors.Is(err, ErrSenderUnauthorized), "finalize bypassed revocation", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_receipt`) == 1,
		"revoked transfer durably accepted",
	)
}

func TestSenderRevocationWaitsForFinalAcceptanceCommit(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	senderOverride(t, f, "example.test")
	proof, err := f.s.AuthorizeSender(ctx, f.org, "sender@example.test")
	requireReceiptNoError(t, err)
	env := outbound(t, f, "recipient@example.test")
	env.MailFrom = "sender@example.test"
	env.Outbound.SenderAuthorization = proof
	atCommit, release := make(chan struct{}), make(chan struct{})
	f.s.db.beforeCommit = func(pgx.Tx) error { close(atCommit); <-release; return nil }
	accepted := make(chan error, 1)
	go func() {
		_, err := f.s.Accept(ctx, newID(t), env, io.NopCloser(strings.NewReader("commit before revocation")))
		accepted <- err
	}()
	select {
	case <-atCommit:
	case <-time.After(5 * time.Second):
		t.Fatal("acceptance did not reach commit")
	}
	revoked := make(chan error, 1)
	go revokeSenderDuringCommit(t, ctx, f, revoked)
	deadline := time.Now().Add(3 * time.Second)
	for scalar(t, f.pool, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND `+
		`wait_event_type='Lock'`, f.pool.Config().ConnConfig.RuntimeParams["application_name"]) == 0 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("revocation did not wait for acceptance share guard")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	err = <-accepted
	requireReceiptNoError(t, err)
	err = <-revoked
	requireReceiptNoError(t, err)
	{
		_, err = f.s.AuthorizeSender(ctx, f.org, "sender@example.test")
		requireReceiptCondition(t, errors.Is(err, ErrSenderUnauthorized), "revocation not visible after commit", err)
	}
}

func revokeSenderDuringCommit(t *testing.T, ctx context.Context, f *fixture, revoked chan<- error) {
	t.Helper()
	func() {
		tx, err := f.pool.Begin(ctx)
		if err == nil {
			defer func() {
				if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
					t.Error(err)
				}
			}()
			err = RevokeSenderGrant(ctx, tx, f.org, "example.test", "manager", "revoke during commit")
			if err == nil {
				err = tx.Commit(ctx)
			}
		}
		revoked <- err
	}()
}

func requireSenderChallengeSecretHashed(t *testing.T, ctx context.Context, f *fixture, challenge SenderChallenge) {
	t.Helper()
	var hash []byte
	var stored string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(
			ctx,
			`SELECT token_hash,row_to_json(c)::text FROM fry_sender_challenge c WHERE id=$1`,
			challenge.ID.String(),
		).
			Scan(&hash, &stored),
	)
	digest := sha256.Sum256([]byte(challenge.TXTValue))
	requireReceiptCondition(t, string(hash) == string(digest[:]), "challenge secret persisted", stored)
	requireReceiptCondition(t, !strings.Contains(stored, challenge.TXTValue), "challenge secret persisted", stored)
	requireReceiptCondition(
		t,
		!strings.Contains(stored, strings.TrimPrefix(challenge.TXTValue, "fry-sender-v1=")),
		"challenge secret persisted",
		stored,
	)
}
