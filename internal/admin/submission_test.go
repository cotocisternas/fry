//go:build linux

package admin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cotocisternas/fry/internal/maildirectory"
)

func TestSubmissionAdministrationScopeRotationAndAudit(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	manager := f.manager(t, false)
	directory := maildirectory.New(f.db)
	accounts := []maildirectory.SubmissionAccount{
		{
			Username:          "assigned-sender",
			Organization:      f.org,
			Enabled:           true,
			AuthorizedDomains: []string{"business.test", "example.test"},
		},
		{
			Username:          "other-sender",
			Organization:      f.other,
			Enabled:           true,
			AuthorizedDomains: []string{"business.test", "example.test"},
		},
	}
	check(t, directory.SeedSubmission(ctx, accounts, []string{"original-send-password", "other-send-password"}))
	old, err := directory.AuthenticateSubmission(ctx, accounts[0].Username, "original-send-password")
	check(t, err)
	requireSubmissionAdministrationScope(t, f, ctx, manager, accounts)
	check(
		t,
		f.service.RotateSubmissionPassword(ctx, manager.Token, f.org, accounts[0].Username, "rotated-send-password"),
	)
	requireErrorIs(t, directory.ValidateSubmission(ctx, old), maildirectory.ErrNotFound)
	fresh, err := directory.AuthenticateSubmission(ctx, accounts[0].Username, "rotated-send-password")
	check(t, err)
	if fresh.Version <= old.Version {
		t.Fatal("rotation did not advance generation")
	}
	check(t, f.service.SetSubmissionAccess(ctx, manager.Token, f.org, accounts[0].Username, false))
	check(
		t,
		f.service.RotateSubmissionPassword(ctx, manager.Token, f.org, accounts[0].Username, "disabled-new-password"),
	)
	{
		_, err := directory.AuthenticateSubmission(ctx, accounts[0].Username, "disabled-new-password")
		requireErrorIs(t, err, maildirectory.ErrNotFound)
	}
	check(t, f.service.SetSubmissionAccess(ctx, manager.Token, f.org, accounts[0].Username, true))
	_, err = directory.AuthenticateSubmission(ctx, accounts[0].Username, "disabled-new-password")
	check(t, err)
	// Send-only issuance cannot create mailbox or administrator access.
	{
		_, err := directory.Authenticate(ctx, accounts[0].Username, "disabled-new-password")
		requireErrorIs(t, err, maildirectory.ErrNotFound)
	}
	{
		_, err := f.service.Login(ctx, accounts[0].Username, "disabled-new-password")
		requireErrorIs(t, err, ErrUnauthorized)
	}
	requireSubmissionAudits(t, f, accounts[0].Username, manager.Identity.ID)
}

func TestSubmissionCreateAndPolicyScopeAudit(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	manager := f.manager(t, false)
	a := maildirectory.SubmissionAccount{
		Username:          "dynamic",
		Organization:      f.org,
		Enabled:           true,
		AuthorizedSenders: []string{"Robot@Example.TEST"},
	}
	check(t, f.service.CreateSubmissionAccount(ctx, manager.Token, a, "new-private-secret"))
	requireErrorIs(
		t,
		f.service.CreateSubmissionAccount(ctx, manager.Token, a, "replacement-secret"),
		maildirectory.ErrConflict,
	)
	a.Organization = f.other
	requireErrorIs(t, f.service.CreateSubmissionAccount(ctx, manager.Token, a, "other-private-secret"), ErrForbidden)
	requireErrorIs(
		t,
		f.service.CreateSubmissionAccount(ctx, f.root.Token, a, "other-private-secret"),
		maildirectory.ErrConflict,
	)
	a.Username = "unknownorg"
	a.Organization = "99999999999999999999999999999999"
	requireErrorIs(t, f.service.CreateSubmissionAccount(ctx, f.root.Token, a, "unknown-org-secret"), ErrInvalid)
	check(t, f.service.SetSubmissionPolicy(ctx, manager.Token, f.org, "dynamic", nil, []string{"New.TEST"}))
	list, err := f.service.SubmissionAccounts(ctx, manager.Token, f.org)
	check(t, err)
	if len(list) != 1 || list[0].Version != 2 || len(list[0].AuthorizedSenders) != 0 ||
		list[0].AuthorizedDomains[0] != "new.test" {
		t.Fatal(list)
	}
	raw, err := json.Marshal(list)
	check(t, err)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "hash") {
		t.Fatal("credential metadata leak", string(raw))
	}
	requireErrorIs(
		t,
		f.service.SetSubmissionPolicy(ctx, manager.Token, f.other, "dynamic", nil, []string{"new.test"}),
		ErrForbidden,
	)
	var count int
	check(
		t,
		f.db.QueryRow(ctx, `SELECT count(*) FROM fry_admin_audit WHERE action IN `+
			`('submission-credential-create','submission-credential-policy-update')`).
			Scan(&count),
	)
	if count != 2 {
		t.Fatal("audit mutation notatomic", count)
	}
}

func requireSubmissionAudits(t *testing.T, f *fixture, username, actor string) {
	t.Helper()
	ctx := context.Background()
	var count int
	check(
		t,
		f.db.QueryRow(ctx, `SELECT count(*) FROM fry_admin_audit WHERE action LIKE 'submission-credential-%'`).
			Scan(&count),
	)
	if count != 4 {
		t.Fatalf("unexpected successful audit count: %d", count)
	}
	check(
		t,
		f.db.QueryRow(ctx, `SELECT count(*) FROM fry_admin_audit WHERE action LIKE `+
			`'submission-credential-%' AND organization=$1 AND target=$2 AND actor=$3`, f.org, username, actor).
			Scan(&count),
	)
	if count != 4 {
		t.Fatalf("wrong audit scope/actor: %d", count)
	}
}

func requireSubmissionAdministrationScope(
	t *testing.T,
	f *fixture,
	ctx context.Context,
	manager Session,
	accounts []maildirectory.SubmissionAccount,
) {
	t.Helper()
	listed, err := f.service.SubmissionAccounts(ctx, manager.Token, f.org)
	check(t, err)
	if len(listed) != 1 || listed[0].Username != accounts[0].Username {
		t.Fatalf("wrong scope: %+v", listed)
	}
	{
		_, err := f.service.SubmissionAccounts(ctx, manager.Token, f.other)
		requireErrorIs(t, err, ErrForbidden)
	}
	requireErrorIs(
		t,
		f.service.RotateSubmissionPassword(ctx, manager.Token, f.other, accounts[1].Username, "unauthorized-password"),
		ErrForbidden,
	)
	requireErrorIs(
		t,
		f.service.SetSubmissionAccess(ctx, manager.Token, f.other, accounts[1].Username, false),
		ErrForbidden,
	)
	requireErrorIs(
		t,
		f.service.RotateSubmissionPassword(ctx, manager.Token, f.org, accounts[1].Username, "wrong-owner-password"),
		maildirectory.ErrNotFound,
	)
	requireErrorIs(
		t,
		f.service.RotateSubmissionPassword(ctx, f.root.Token, f.org, "not-configured", "new-password"),
		maildirectory.ErrNotFound,
	)
}
