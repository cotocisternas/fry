//go:build linux

package admin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestContentOnlyRevocationPreservesAuthorizedTransferAndMetadata(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	manager := f.manager(t, true)
	receipt, raw := f.receive(t)
	openCtx, cancelOpen := context.WithCancel(ctx)
	defer cancelOpen()
	content, err := f.service.OpenContent(openCtx, manager.Token, f.org, receipt)
	check(t, err)
	t.Cleanup(func() { check(t, content.Close()) })
	prefix := make([]byte, 8)
	if _, err := io.ReadFull(content, prefix); err != nil {
		t.Fatal(err)
	}

	check(t, f.service.ReplaceGrants(ctx, f.root.Token, manager.Identity.ID, []Grant{{Scope: f.org, Content: false}}))
	// Each subsequent request checks the new grant, while the existing handle
	// retains the permission committed when that transfer was authorized.
	freshCtx, cancelFresh := context.WithCancel(ctx)
	defer cancelFresh()
	fresh, err := f.service.OpenContent(freshCtx, manager.Token, f.org, receipt)
	if fresh != nil {
		check(t, fresh.Close())
		t.Fatal("content-only revocation returned a fresh content handle")
	}
	requireErrorIs(t, err, ErrForbidden)
	messages, err := f.service.Messages(t.Context(), manager.Token, f.org, "")
	check(t, err)
	if len(messages) != 1 || messages[0].ID != receipt || messages[0].Size != int64(len(raw)) {
		t.Fatalf("content-only revocation changed metadata access: %+v", messages)
	}
	remainder, err := io.ReadAll(content)
	check(t, err)
	check(t, content.Close())
	got := bytes.Join([][]byte{prefix, remainder}, nil)
	if !bytes.Equal(got, raw) {
		t.Fatal("previously authorized transfer changed after content-only revocation")
	}
	events, err := f.service.Audits(t.Context(), manager.Token, 0)
	check(t, err)
	requireDownloadOutcome(t, events, receipt, "read-complete", 1)
}

// SQL is used only to inject an audit failure; all resulting state is observed
// through application interfaces against the real, schema-isolated database.
func rejectMutationAudit(t *testing.T, f *fixture) func() {
	t.Helper()
	_, err := f.db.Exec(t.Context(), `
CREATE FUNCTION reject_mutation_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'synthetic mutation audit failure';
END $$;
CREATE TRIGGER reject_mutation_audit BEFORE INSERT ON fry_admin_audit
FOR EACH ROW WHEN (NEW.action IN ('inbox-create', 'alias-update'))
EXECUTE FUNCTION reject_mutation_audit()`)
	check(t, err)
	remove := func() {
		// Cleanup runs after the test context is canceled and must still remove
		// the fault before the fixture closes its database pool.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := f.db.Exec(
			ctx,
			`DROP TRIGGER IF EXISTS reject_mutation_audit ON fry_admin_audit; DROP FUNCTION `+
				`IF EXISTS reject_mutation_audit()`,
		)
		if err != nil {
			t.Errorf("remove mutation audit fault: %v", err)
		}
	}
	t.Cleanup(remove)
	return remove
}

func requireMutationAuditFailure(t *testing.T, err error) {
	t.Helper()
	var failure *pgconn.PgError
	if !errors.As(err, &failure) || failure.Code != "P0001" || failure.Message != "synthetic mutation audit failure" {
		t.Fatalf("operation did not reach the injected audit failure: %v", err)
	}
}

func TestInboxCreationRollsBackWhenMutationAuditFails(t *testing.T) {
	f := setup(t)
	manager := f.manager(t, false)
	inbox := Inbox{ID: newID(t), Organization: f.org}
	removeFault := rejectMutationAudit(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	requireMutationAuditFailure(t, f.service.CreateInbox(ctx, manager.Token, inbox))
	cancel()
	inboxes, err := f.service.Inboxes(t.Context(), manager.Token, f.org)
	check(t, err)
	if len(inboxes) != 1 || inboxes[0].ID != f.inbox {
		t.Fatalf("failed audited creation changed inbox listing: %+v", inboxes)
	}
	events, err := f.service.Audits(t.Context(), manager.Token, 0)
	check(t, err)
	for _, event := range events {
		if event.Action == "inbox-create" && event.Target == inbox.ID {
			t.Fatalf("failed creation left a committed audit: %+v", event)
		}
	}

	removeFault()
	retryCtx, cancelRetry := context.WithCancel(t.Context())
	defer cancelRetry()
	check(t, f.service.CreateInbox(retryCtx, manager.Token, inbox))
	inboxes, err = f.service.Inboxes(t.Context(), manager.Token, f.org)
	check(t, err)
	if len(inboxes) != 2 {
		t.Fatalf("successful retry did not add exactly one inbox: %+v", inboxes)
	}
	requireInboxPresent(t, inboxes, inbox.ID, f.org)
	events, err = f.service.Audits(t.Context(), manager.Token, 0)
	check(t, err)
	committed := committedAuditCount(events, "inbox-create", inbox.ID)
	if committed != 1 {
		t.Fatalf("inbox retry committed audit count: %d", committed)
	}
}

func TestAliasUpdateRollsBackListingAndRoutingWhenMutationAuditFails(t *testing.T) {
	f := setup(t)
	manager := f.manager(t, false)
	ctx := t.Context()
	replacementInbox := newID(t)
	check(t, f.service.CreateInbox(ctx, manager.Token, Inbox{ID: replacementInbox, Organization: f.org}))
	check(
		t,
		f.service.PutDomain(
			ctx,
			manager.Token,
			maildirectory.Domain{Name: "audit-rollback.test", Organization: f.org, Enabled: true},
		),
	)
	original := maildirectory.Alias{
		Address:      "invoices@audit-rollback.test",
		Organization: f.org,
		Inboxes:      []string{f.inbox},
		Enabled:      true,
	}
	check(t, f.service.PutAlias(ctx, manager.Token, original))
	directory := maildirectory.New(f.db)

	assertAliasState(t, f, manager.Token, directory, original)
	before, err := f.service.Audits(ctx, manager.Token, 0)
	check(t, err)
	updated := original
	updated.Inboxes = []string{replacementInbox}
	removeFault := rejectMutationAudit(t, f)
	updateCtx, cancelUpdate := context.WithCancel(t.Context())
	defer cancelUpdate()
	requireMutationAuditFailure(t, f.service.PutAlias(updateCtx, manager.Token, updated))
	cancelUpdate()
	assertAliasState(t, f, manager.Token, directory, original)
	after, err := f.service.Audits(t.Context(), manager.Token, 0)
	check(t, err)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("failed alias update changed committed audit history: before %+v, after %+v", before, after)
	}

	removeFault()
	retryCtx, cancelRetry := context.WithCancel(t.Context())
	defer cancelRetry()
	check(t, f.service.PutAlias(retryCtx, manager.Token, updated))
	assertAliasState(t, f, manager.Token, directory, updated)
	events, err := f.service.Audits(t.Context(), manager.Token, 0)
	check(t, err)
	committed := committedAuditCount(events, "alias-update", original.Address)
	if committed != 2 {
		t.Fatalf("original alias and successful retry committed audit count: %d", committed)
	}
}

func TestCrossOrganizationContentDenialReturnsNoHandleOrDownloadAudit(t *testing.T) {
	f := setup(t)
	manager := f.manager(t, true)
	other := *f
	other.org = f.other
	other.inbox = newID(t)
	check(t, f.service.CreateInbox(t.Context(), f.root.Token, Inbox{ID: other.inbox, Organization: other.org}))
	receipt, _ := other.receive(t)

	for _, tc := range []struct {
		name string
		org  string
		want error
	}{
		{name: "actual organization is forbidden", org: f.other, want: ErrForbidden},
		{name: "forged assigned organization hides receipt", org: f.org, want: receiptstore.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			content, err := f.service.OpenContent(ctx, manager.Token, tc.org, receipt)
			if content != nil {
				check(t, content.Close())
				t.Fatal("cross-organization content request returned a handle")
			}
			requireErrorIs(t, err, tc.want)
			// The operator can see both organizations, so audit filtering cannot
			// hide an accidentally authorized download in the other organization.
			events, err := f.service.Audits(t.Context(), f.root.Token, 0)
			check(t, err)
			for _, event := range events {
				if event.Action == "content-download" && event.Target == receipt {
					t.Fatalf("denied cross-organization request created a download audit: %+v", event)
				}
			}
		})
	}
}

func requireDownloadOutcome(t *testing.T, events []Audit, receipt, outcome string, want int) {
	t.Helper()
	count := 0
	for _, event := range events {
		if event.Action == "content-download" && event.Target == receipt {
			count++
			if event.Outcome != outcome {
				t.Fatalf("download outcome=%s, want %s", event.Outcome, outcome)
			}
		}
	}
	if count != want {
		t.Fatalf("download audit count=%d, want %d", count, want)
	}
}

func committedAuditCount(events []Audit, action, target string) int {
	count := 0
	for _, event := range events {
		if event.Action == action && event.Target == target && event.Outcome == "committed" {
			count++
		}
	}
	return count
}

func assertAliasState(
	t *testing.T,
	f *fixture,
	token string,
	directory *maildirectory.Directory,
	want maildirectory.Alias,
) {
	t.Helper()
	aliases, err := f.service.Aliases(t.Context(), token, f.org)
	check(t, err)
	if !reflect.DeepEqual(aliases, []maildirectory.Alias{want}) {
		t.Fatalf("alias listing: got %+v, want %+v", aliases, want)
	}
	route, err := directory.Resolve(t.Context(), want.Address)
	check(t, err)
	if route.Organization.String() != f.org || len(route.Inboxes) != 1 || route.Inboxes[0].String() != want.Inboxes[0] {
		t.Fatalf("alias route: got %+v, want organization %s and inbox %s", route, f.org, want.Inboxes[0])
	}
}

func requireInboxPresent(t *testing.T, inboxes []Inbox, id, org string) {
	t.Helper()
	found := false
	for _, got := range inboxes {
		if got.ID == id && got.Organization == org {
			found = true
		}
	}
	if !found {
		t.Fatalf("successful retry omitted the new inbox: %+v", inboxes)
	}
}
