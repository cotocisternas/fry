//go:build linux

package admin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	service           *Service
	db                *pgxpool.Pool
	mail              *receiptstore.Store
	root              Session
	org, other, inbox string
}

func newID(t *testing.T) string {
	t.Helper()
	id, e := identifier()
	if e != nil {
		t.Fatal(e)
	}
	return id
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db := setupDatabase(t, ctx)
	check(t, receiptstore.Migrate(ctx, db))
	check(t, Migrate(ctx, db))
	var members [2]*receiptstore.LocalMember
	for i := range members {
		id, idErr := mailID(newID(t))
		check(t, idErr)
		m, e := receiptstore.OpenLocalMember(
			ctx,
			db,
			id,
			fmt.Sprint("site-", i),
			t.TempDir(),
			hotstore.Options{MaxMessageBytes: 1 << 20, MaxBytes: 16 << 20, MaxConcurrentAppends: 4},
		)
		check(t, e)
		members[i] = m
		t.Cleanup(func() {
			if err := m.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	mail, err := receiptstore.New(
		ctx,
		db,
		members,
		receiptstore.Options{Lease: time.Second, RecoveryRetention: time.Hour},
	)
	check(t, err)
	svc, err := New(db, mail, time.Hour)
	check(t, err)
	created, err := svc.Bootstrap(ctx, "operator", "test-operator-secret", true)
	check(t, err)
	if !created {
		t.Fatal("bootstrap did not create operator")
	}
	root, err := svc.Login(ctx, "operator", "test-operator-secret")
	check(t, err)
	f := &fixture{svc, db, mail, root, newID(t), newID(t), newID(t)}
	check(t, svc.PutOrganization(ctx, root.Token, Organization{f.org, "Assigned organization"}))
	check(t, svc.PutOrganization(ctx, root.Token, Organization{f.other, "Other organization"}))
	check(t, svc.CreateInbox(ctx, root.Token, Inbox{ID: f.inbox, Organization: f.org}))
	return f
}

func (f *fixture) manager(t *testing.T, content bool) Session {
	t.Helper()
	ctx := context.Background()
	id, err := f.service.PutUser(ctx, f.root.Token, "manager", "test-manager-secret", true)
	check(t, err)
	check(t, f.service.ReplaceGrants(ctx, f.root.Token, id, []Grant{{Scope: f.org, Content: content}}))
	session, err := f.service.Login(ctx, "manager", "test-manager-secret")
	check(t, err)
	return session
}

func TestPersistentAccountsIsolationAndImmediateRevocation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	manager := f.manager(t, false)
	orgs, err := f.service.Organizations(ctx, manager.Token)
	check(t, err)
	if len(orgs) != 1 || orgs[0].ID != f.org {
		t.Fatalf("leaked organization: %+v", orgs)
	}
	{
		_, err := f.service.Inboxes(ctx, manager.Token, f.other)
		requireErrorIs(t, err, ErrForbidden)
	}
	requireErrorIs(
		t,
		f.service.ReplaceGrants(ctx, manager.Token, manager.Identity.ID, []Grant{{Scope: "*"}}),
		ErrForbidden,
	)
	check(t, f.service.PutOrganization(ctx, manager.Token, Organization{f.org, "Renamed"}))
	requireErrorIs(
		t,
		f.service.PutOrganization(ctx, manager.Token, Organization{newID(t), "Unauthorized"}),
		ErrForbidden,
	)
	restarted, err := New(f.db, f.mail, time.Hour)
	check(t, err)
	created, err := restarted.Bootstrap(ctx, "operator", "different-password", true)
	check(t, err)
	if created {
		t.Fatal("bootstrap reset existing installation")
	}
	{
		_, err := restarted.Login(ctx, "operator", "different-password")
		requireErrorIs(t, err, ErrUnauthorized)
	}
	_, err = restarted.Identity(ctx, manager.Token)
	check(t, err)
	check(t, restarted.ReplaceGrants(ctx, f.root.Token, manager.Identity.ID, nil))
	{
		_, err := restarted.Identity(ctx, manager.Token)
		requireErrorIs(t, err, ErrForbidden)
	}
	check(t, restarted.ReplaceGrants(ctx, f.root.Token, manager.Identity.ID, []Grant{{Scope: f.org}}))
	_, err = restarted.PutUser(ctx, f.root.Token, "manager", "replacement-password", true)
	check(t, err)
	{
		_, err := restarted.Identity(ctx, manager.Token)
		requireErrorIs(t, err, ErrUnauthorized)
	}
	fresh, err := restarted.Login(ctx, "manager", "replacement-password")
	check(t, err)
	check(t, restarted.Logout(ctx, fresh.Token))
	{
		_, err := restarted.Identity(ctx, fresh.Token)
		requireErrorIs(t, err, ErrUnauthorized)
	}
}

func (f *fixture) receive(t *testing.T) (string, []byte) {
	t.Helper()
	ctx := context.Background()
	org, idErr := mailID(f.org)
	check(t, idErr)
	inbox, idErr := mailID(f.inbox)
	check(t, idErr)
	attempt, idErr := mailID(newID(t))
	check(t, idErr)
	raw := []byte("Subject: confidential\r\n\r\nfixture attachment bytes\r\n")
	receipt, err := f.mail.Accept(
		ctx,
		attempt,
		receiptstore.Envelope{
			Organization: org,
			MailFrom:     "sender@example.test",
			Destinations: []receiptstore.Destination{{Inbox: inbox, Recipients: []string{"inbox@example.test"}}},
		},
		io.NopCloser(bytes.NewReader(raw)),
	)
	check(t, err)
	return receipt.ID.String(), raw
}

func TestContentPermissionAndAuditBeforeBytes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	manager := f.manager(t, false)
	receipt, raw := f.receive(t)
	{
		_, err := f.service.OpenContent(ctx, manager.Token, f.org, receipt)
		requireErrorIs(t, err, ErrForbidden)
	}
	check(t, f.service.ReplaceGrants(ctx, f.root.Token, manager.Identity.ID, []Grant{{Scope: f.org, Content: true}}))
	content, err := f.service.OpenContent(ctx, manager.Token, f.org, receipt)
	check(t, err)
	var count int
	check(
		t,
		f.db.QueryRow(ctx, `SELECT count(*) FROM fry_admin_audit WHERE action='content-download' AND `+
			`outcome='authorized'`).
			Scan(&count),
	)
	if count != 1 {
		t.Fatal("content reader exposed before committed audit")
	}
	got, err := io.ReadAll(content)
	check(t, err)
	check(t, content.Close())
	if !bytes.Equal(got, raw) {
		t.Fatal("content changed")
	}
	events, err := f.service.Audits(ctx, manager.Token, 0)
	check(t, err)
	found := false
	for _, a := range events {
		if a.Organization != f.org {
			t.Fatal("audit leaked other organization")
		}
		if a.Action == "content-download" && a.Outcome == "read-complete" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing successful content audit")
	}
	// A broken audit sink must prevent returning any content handle.
	_, err = f.db.Exec(ctx, `ALTER TABLE fry_admin_audit RENAME TO disabled_audit`)
	check(t, err)
	if reader, err := f.service.OpenContent(ctx, manager.Token, f.org, receipt); err == nil || reader != nil {
		t.Fatal("audit outage did not fail closed")
	}
}

func requireErrorIs(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("operation error=%v, want %v", err, want)
	}
}

func setupDatabase(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("FRY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("run scripts/tests/receipts.sh for PostgreSQL-backed administration tests")
	}
	control, err := pgxpool.New(ctx, dsn)
	check(t, err)
	t.Cleanup(control.Close)
	schema := "fry_admin_" + newID(t)
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = control.Exec(ctx, "CREATE SCHEMA "+quoted)
	check(t, err)
	t.Cleanup(func() {
		_, err := control.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
		if err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	check(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	check(t, err)
	t.Cleanup(db.Close)
	return db
}
