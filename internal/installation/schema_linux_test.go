//go:build linux

package installation_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/admin"
	"github.com/cotocisternas/fry/internal/dbschema"
	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/installation"
	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/replicaconfig"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func installationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("FRY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL via scripts/tests/receipts.sh")
	}
	ctx := t.Context()
	control, err := pgxpool.New(ctx, dsn)
	checkCommand(t, err)
	t.Cleanup(control.Close)
	id, err := hotstore.NewID()
	checkCommand(t, err)
	schema := pgx.Identifier{"installation_" + id.String()}.Sanitize()
	{
		_, err := control.Exec(ctx, "CREATE SCHEMA "+schema)
		checkCommand(t, err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := control.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	checkCommand(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = "installation_" + id.String()
	checkCommand(t, dbschema.ConfigurePool(cfg))
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	checkCommand(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestInstallationComposesAdministrationDirectoryAndReceipts(t *testing.T) {
	pool := installationPool(t)
	ctx := t.Context()
	checkCommand(t, installation.Initialize(ctx, pool))
	checkCommand(t, installation.Check(ctx, pool))
	store := installedStore(t, ctx, pool)
	service, err := admin.New(pool, store, time.Hour)
	checkCommand(t, err)
	created, err := service.Bootstrap(ctx, "operator", "installation-fixture-password", true)
	if err != nil || !created {
		t.Fatalf("bootstrap: created=%v err=%v", created, err)
	}
	session, err := service.Login(ctx, "operator", "installation-fixture-password")
	checkCommand(t, err)
	org := receiptstore.ID{15: 3}
	inbox := receiptstore.ID{15: 4}
	checkCommand(
		t,
		service.PutOrganization(ctx, session.Token, admin.Organization{ID: org.String(), Name: "Fixture organization"}),
	)
	checkCommand(
		t,
		service.CreateInbox(ctx, session.Token, admin.Inbox{ID: inbox.String(), Organization: org.String()}),
	)
	directory := installedDirectory(t, ctx, pool, org, inbox)
	route, err := directory.Resolve(ctx, "original@example.test")
	checkCommand(t, err)
	if route.Organization != org || len(route.Inboxes) != 1 || route.Inboxes[0] != inbox {
		t.Fatalf("route=%+v", route)
	}
	const original = "From: sender@example.test\r\nSubject: installed\r\n\r\nretained original\r\n"
	attempt := receiptstore.ID{15: 5}
	receipt, err := store.Accept(
		ctx,
		attempt,
		receiptstore.Envelope{
			Organization: org,
			MailFrom:     "sender@example.test",
			Destinations: []receiptstore.Destination{{Inbox: inbox, Recipients: []string{"original@example.test"}}},
		},
		io.NopCloser(bytes.NewBufferString(original)),
	)
	checkCommand(t, err)
	// Rechecking a live installation must preserve authority and accepted bytes.
	checkCommand(t, installation.Check(ctx, pool))
	if err := installation.Initialize(ctx, pool); err == nil {
		t.Fatal("existing installation silently adopted")
	}
	{
		_, err := service.Identity(ctx, session.Token)
		checkCommand(t, err)
	}
	{
		_, err := directory.Resolve(ctx, "original@example.test")
		checkCommand(t, err)
	}
	var got bytes.Buffer
	checkCommand(t, store.Read(ctx, org, receipt.ID, 0, receipt.Size, &got))
	if got.String() != original {
		t.Fatalf("retained original=%q", got.String())
	}
}

func TestInstallationRejectsIncompleteAndIncompatibleSchemas(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(context.Context, *pgxpool.Pool) error
	}{
		{
			"empty",
			func(context.Context, *pgxpool.Pool) error { return nil },
		},
		{"receipt component only", func(ctx context.Context, p *pgxpool.Pool) error {
			return dbschema.Initialize(ctx, p, []dbschema.Component{receiptstore.SchemaDefinition()})
		}},
		{"administration component missing", func(ctx context.Context, p *pgxpool.Pool) error {
			if err := installation.Initialize(ctx, p); err != nil {
				return err
			}
			_, err := p.Exec(ctx, `DROP TABLE fry_admin_session`)
			return err
		}},
		{"directory component missing", func(ctx context.Context, p *pgxpool.Pool) error {
			if err := installation.Initialize(ctx, p); err != nil {
				return err
			}
			_, err := p.Exec(ctx, `DROP TABLE fry_mail_account`)
			return err
		}},
		{"receipt component incompatible", func(ctx context.Context, p *pgxpool.Pool) error {
			if err := installation.Initialize(ctx, p); err != nil {
				return err
			}
			_, err := p.Exec(ctx, `ALTER TABLE fry_member_file DROP COLUMN charged_bytes`)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := installationPool(t)
			checkCommand(t, tc.prepare(t.Context(), pool))
			if err := installation.Check(t.Context(), pool); err == nil {
				t.Fatalf("incompatible installation: %v", err)
			}
		})
	}
}

func installedStore(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *receiptstore.Store {
	t.Helper()
	cfg := replicaconfig.Storage{
		DataDirectory:          t.TempDir(),
		RecoveryRetentionHours: 24,
		MaxMessageBytes:        1 << 20,
		MemberCapacityBytes:    16 << 20,
		MaxInFlight:            2,
		Members: []replicaconfig.MemberConfig{
			{ID: "00000000000000000000000000000001", Domain: "zone-a"},
			{ID: "00000000000000000000000000000002", Domain: "zone-b"},
		},
	}
	store, closeStore, err := cfg.Open(ctx, pool)
	checkCommand(t, err)
	t.Cleanup(func() {
		if err := closeStore(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func installedDirectory(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	org, inbox receiptstore.ID,
) *maildirectory.Directory {
	t.Helper()
	directory := maildirectory.New(pool)
	checkCommand(
		t,
		directory.Seed(
			ctx,
			[]maildirectory.Domain{
				{
					Name:         "example.test",
					Organization: org.String(),
					CatchAll:     inbox.String(),
					Enabled:      true,
				},
			},
			nil,
			nil,
			nil,
		),
	)
	return directory
}
