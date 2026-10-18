//go:build linux

package admintransport

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/admin"
	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	service           *admin.Service
	db                *pgxpool.Pool
	mail              *receiptstore.Store
	member            string
	root              admin.Session
	org, other, inbox string
}

func newID(t *testing.T) string {
	t.Helper()
	id, e := hotstore.NewID()
	if e != nil {
		t.Fatal(e)
	}
	return id.String()
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) *fixture {
	t.Helper()
	return setupWithDNS(t, nil)
}

func setupWithDNS(t *testing.T, resolver admin.TXTResolver) *fixture {
	t.Helper()
	ctx := t.Context()
	db := setupDatabase(t, ctx)
	check(t, receiptstore.Migrate(ctx, db))
	check(t, admin.Migrate(ctx, db))
	var firstMember string
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
		if i == 0 {
			firstMember = id.String()
		}
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
	svc, err := admin.NewWithOptions(db, mail, time.Hour, admin.Options{SenderDNS: resolver})
	check(t, err)
	created, err := svc.Bootstrap(ctx, "operator", "test-operator-secret", true)
	check(t, err)
	if !created {
		t.Fatal("bootstrap did not create operator")
	}
	root, err := svc.Login(ctx, "operator", "test-operator-secret")
	check(t, err)
	f := &fixture{
		service: svc,
		db:      db,
		mail:    mail,
		member:  firstMember,
		root:    root,
		org:     newID(t),
		other:   newID(t),
		inbox:   newID(t),
	}
	check(t, svc.PutOrganization(ctx, root.Token, admin.Organization{ID: f.org, Name: "Assigned organization"}))
	check(t, svc.PutOrganization(ctx, root.Token, admin.Organization{ID: f.other, Name: "Other organization"}))
	check(t, svc.CreateInbox(ctx, root.Token, admin.Inbox{ID: f.inbox, Organization: f.org}))
	return f
}

func (f *fixture) manager(t *testing.T, content bool) admin.Session {
	t.Helper()
	ctx := context.Background()
	id, err := f.service.PutUser(ctx, f.root.Token, "manager", "test-manager-secret", true)
	check(t, err)
	check(t, f.service.ReplaceGrants(ctx, f.root.Token, id, []admin.Grant{{Scope: f.org, Content: content}}))
	session, err := f.service.Login(ctx, "manager", "test-manager-secret")
	check(t, err)
	return session
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

type senderDNSFunc func(context.Context, string) ([]string, error)

func (f senderDNSFunc) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return f(ctx, name)
}

func mailID(raw string) (receiptstore.ID, error) {
	var id receiptstore.ID
	b, err := hex.DecodeString(raw)
	if err != nil || len(b) != len(id) {
		return id, admin.ErrInvalid
	}
	copy(id[:], b)
	return id, nil
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
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, err := control.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE")
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
