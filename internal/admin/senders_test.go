//go:build linux

package admin

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

type senderDNSFunc func(context.Context, string) ([]string, error)

func (f senderDNSFunc) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return f(ctx, name)
}

func TestSenderScopeIsolationAndDNSLifecycle(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	manager := f.manager(t, false)
	dns := newSenderDNSState(t, f)
	challenge, err := f.service.SenderChallenge(ctx, manager.Token, f.org, "example.test")
	check(t, err)
	dns.answer = challenge.TXTValue
	{
		_, err := f.service.VerifySender(ctx, manager.Token, f.other, challenge.ID)
		requireErrorIs(t, err, ErrForbidden)
	}
	{
		_, err := f.service.VerifySender(ctx, f.root.Token, newID(t), challenge.ID)
		requireErrorIs(t, err, ErrInvalid)
	}
	if dns.calls.Load() != 0 {
		t.Fatal("unauthorized DNS query")
	}
	dns.fail = true
	if _, err = f.service.VerifySender(ctx, manager.Token, f.org, challenge.ID); err == nil {
		t.Fatal("DNS failure granted authority")
	}
	values, err := f.service.SenderGrants(ctx, manager.Token, f.org)
	check(t, err)
	for _, v := range values {
		if v.Active {
			t.Fatalf("grant on failed DNS: %+v", values)
		}
	}
	dns.fail = false
	dns.answer = "wrong-token"
	if _, err = f.service.VerifySender(ctx, manager.Token, f.org, challenge.ID); err == nil {
		t.Fatal("mismatched TXT granted authority")
	}
	dns.answer = challenge.TXTValue
	grant, err := f.service.VerifySender(ctx, manager.Token, f.org, challenge.ID)
	check(t, err)
	if !grant.Active || grant.Scope != "example.test" || grant.Actor != manager.Identity.ID {
		t.Fatalf("wrong verified grant %+v", grant)
	}
	if _, err = f.service.VerifySender(ctx, manager.Token, f.org, challenge.ID); err == nil {
		t.Fatal("challenge replay accepted")
	}
	checkSenderRevocation(t, f, manager)
}

func TestSenderOverrideAuthorityAndAtomicAudit(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	manager := f.manager(t, false)
	{
		_, err := f.service.OverrideSender(ctx, manager.Token, f.org, "example.test", "approved exception")
		requireErrorIs(t, err, ErrForbidden)
	}
	{
		_, err := f.service.OverrideSender(ctx, f.root.Token, f.org, "example.test", "")
		requireErrorIs(t, err, ErrInvalid)
	}
	grant, err := f.service.OverrideSender(
		ctx,
		f.root.Token,
		f.org,
		"example.test",
		"Approved after external contract review",
	)
	check(t, err)
	if !grant.Active || grant.Reason != "Approved after external contract review" || grant.Actor != f.root.Identity.ID {
		t.Fatalf("override evidence %+v", grant)
	}
	var n int
	check(
		t,
		f.db.QueryRow(ctx, `SELECT count(*) FROM fry_admin_audit WHERE action='sender-grant-override' AND `+
			`actor=$1 AND organization=$2 AND target=$3`, f.root.Identity.ID, f.org, grant.ID).
			Scan(&n),
	)
	if n != 1 {
		t.Fatal("override audit absent")
	}
	_, err = f.db.Exec(
		ctx,
		`CREATE FUNCTION reject_sender_audit() RETURNS trigger LANGUAGE plpgsql AS $$ `+
			`BEGIN IF NEW.action='sender-grant-override' THEN RAISE EXCEPTION 'synthetic `+
			`audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_sender_audit `+
			`BEFORE INSERT ON fry_admin_audit FOR EACH ROW EXECUTE FUNCTION `+
			`reject_sender_audit()`,
	)
	check(t, err)
	if _, err = f.service.OverrideSender(ctx, f.root.Token, f.org, "another.test", "synthetic rollback"); err == nil {
		t.Fatal("audit failure ignored")
	}
	values, err := f.service.SenderGrants(ctx, f.root.Token, f.org)
	check(t, err)
	if len(values) != 1 {
		t.Fatalf("grant survived failed audit %+v", values)
	}
}

func TestSenderDNSReauthorizesAndRevocationWins(t *testing.T) {
	for _, kind := range []string{"permissions", "grant", "supersede"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			manager := f.manager(t, false)
			challenge, err := f.service.SenderChallenge(ctx, manager.Token, f.org, "example.test")
			check(t, err)
			f.service.senderDNS = senderDNSFunc(func(context.Context, string) ([]string, error) {
				switch kind {
				case "permissions":
					check(t, f.service.ReplaceGrants(ctx, f.root.Token, manager.Identity.ID, []Grant{{Scope: f.other}}))
				case "grant":
					check(
						t,
						f.service.RevokeSender(
							ctx,
							f.root.Token,
							f.org,
							"example.test",
							"revoked while lookup pending",
						),
					)
				case "supersede":
					_, err := f.service.SenderChallenge(ctx, f.root.Token, f.org, "example.test")
					check(t, err)
				}
				return []string{challenge.TXTValue}, nil
			})
			if _, err = f.service.VerifySender(ctx, manager.Token, f.org, challenge.ID); err == nil {
				t.Fatal("in-flight verification ignored revocation/supersession")
			}
			org, idErr := mailID(f.org)
			check(t, idErr)
			if _, err = f.mail.AuthorizeSender(ctx, org, "sender@example.test"); err == nil {
				t.Fatal("revoked proof granted authority")
			}
		})
	}
}

func checkSenderRevocation(t *testing.T, f *fixture, manager Session) {
	t.Helper()
	ctx := context.Background()
	check(t, f.service.RevokeSender(ctx, manager.Token, f.org, "example.test", ""))
	values, err := f.service.SenderGrants(ctx, manager.Token, f.org)
	check(t, err)
	if len(values) != 1 || values[0].Active || values[0].Reason != "Administrator revoked sender grant" {
		t.Fatalf("revoke missing %+v", values)
	}
	org, idErr := mailID(f.org)
	check(t, idErr)
	if _, err = f.mail.AuthorizeSender(ctx, org, "sender@example.test"); err == nil {
		t.Fatal("revoked grant authorizes sender")
	}
	{
		_, err := f.service.SenderGrants(ctx, manager.Token, f.other)
		requireErrorIs(t, err, ErrForbidden)
	}
}

type senderDNSState struct {
	calls   atomic.Int64
	answer  string
	fail    bool
	fixture *fixture
	test    *testing.T
}

func newSenderDNSState(t *testing.T, f *fixture) *senderDNSState {
	t.Helper()
	state := &senderDNSState{fixture: f, test: t}
	f.service.senderDNS = senderDNSFunc(state.lookup)
	return state
}

func (state *senderDNSState) lookup(ctx context.Context, _ string) ([]string, error) {
	state.calls.Add(1)
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
		state.test.Error("DNS lookup missing bounded deadline")
	}
	if state.fixture.db.Stat().AcquiredConns() != 0 {
		state.test.Error("transaction held during DNS")
	}
	if state.fail {
		return nil, fmt.Errorf("synthetic DNS failure")
	}
	return []string{state.answer}, nil
}
