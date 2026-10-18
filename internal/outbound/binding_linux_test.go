//go:build linux

package outbound

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func bindingID(t *testing.T) receiptstore.ID {
	t.Helper()
	id, err := hotstore.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func bindingStore(t *testing.T) (*receiptstore.Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("FRY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("run scripts/tests/receipts.sh for PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := pgx.Identifier{"fry_relay_" + bindingID(t).String()}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = strings.Trim(schema, "\"")
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = receiptstore.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	members := bindingMembers(t, ctx, pool)
	store, err := receiptstore.New(
		ctx,
		pool,
		members,
		receiptstore.Options{Lease: time.Minute, RecoveryRetention: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	return store, pool
}

func bindingMembers(t *testing.T, ctx context.Context, pool *pgxpool.Pool) [2]*receiptstore.LocalMember {
	t.Helper()
	var members [2]*receiptstore.LocalMember
	for i := range members {
		member, err := receiptstore.OpenLocalMember(
			ctx,
			pool,
			bindingID(t),
			[]string{"a", "b"}[i],
			t.TempDir(),
			hotstore.Options{MaxMessageBytes: 1 << 20, MaxBytes: 8 << 20, MaxConcurrentAppends: 2},
		)
		if err != nil {
			t.Fatal(err)
		}
		members[i] = member
		t.Cleanup(func() {
			if err := member.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	return members
}

func TestWorkerRelayBindingRejectsRedirectAndPermitsCredentialRotation(t *testing.T) {
	store, pool := bindingStore(t)
	ctx := context.Background()
	relay := Relay{
		ID:        bindingID(t),
		Address:   "relay.example.test:587",
		Transport: "starttls",
		Username:  "submission-account",
		Password:  "initial-password-secret",
	}
	if _, err := New(store, "fry.test", []Relay{relay}); err != nil {
		t.Fatal(err)
	}
	envelope := receiptstore.Envelope{
		Organization: bindingID(t),
		MailFrom:     "sender@example.test",
		Outbound: &receiptstore.Outbound{
			Relay:       relay.ID,
			Recipients:  []string{"recipient@example.test"},
			RetryWindow: time.Hour,
		},
	}
	if _, err := store.Accept(
		ctx,
		bindingID(t),
		envelope,
		io.NopCloser(strings.NewReader("Subject: accepted\r\n\r\noriginal")),
	); err != nil {
		t.Fatal(err)
	}
	redirected := relay
	redirected.Address = "another-relay.example.test:587"
	if _, err := New(store, "fry.test", []Relay{redirected}); !errors.Is(err, receiptstore.ErrRelayBinding) {
		t.Fatal("restart redirected accepted relay ID", err)
	}
	rotated := relay
	rotated.Password = "rotated-password-secret"
	rotated.Roots = x509.NewCertPool()
	worker, err := New(store, "fry.test", []Relay{rotated})
	if err != nil {
		t.Fatal("credential rotation rejected", err)
	}
	if worker.relays[relay.ID].Password != rotated.Password || worker.relays[relay.ID].Address != relay.Address {
		t.Fatal("worker did not retain intended destination and new credential")
	}
	requirePrivateRelayBinding(t, ctx, pool, relay, rotated)
	other := relay
	other.ID = bindingID(t)
	if _, err = New(store, "fry.test", []Relay{other}); !errors.Is(err, receiptstore.ErrRelayBinding) {
		t.Fatal("startup omitted active selected destination", err)
	}
}

func requirePrivateRelayBinding(t *testing.T, ctx context.Context, pool *pgxpool.Pool, relay, rotated Relay) {
	t.Helper()
	var persisted string
	if err := pool.QueryRow(
		ctx,
		`SELECT row_to_json(binding)::text FROM fry_relay_binding binding WHERE id=$1`,
		relay.ID.String(),
	).
		Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(persisted, relay.Password) || strings.Contains(persisted, rotated.Password) ||
		strings.Contains(persisted, "password") ||
		strings.Contains(persisted, "roots") {
		t.Fatal("credentials or trust material persisted", persisted)
	}
}
