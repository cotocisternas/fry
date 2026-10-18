//go:build linux

package admin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type memberStorageQueryTracer struct {
	armed   atomic.Bool
	calls   atomic.Int32
	pause   bool
	paused  atomic.Bool
	entered chan struct{}
	resume  chan struct{}
}

func (q *memberStorageQueryTracer) TraceQueryStart(
	ctx context.Context,
	_ *pgx.Conn,
	data pgx.TraceQueryStartData,
) context.Context {
	if q.armed.Load() && strings.Contains(data.SQL, "FROM fry_member m ") {
		q.calls.Add(1)
		if q.pause && q.paused.CompareAndSwap(false, true) {
			close(q.entered)
			select {
			case <-q.resume:
			case <-ctx.Done():
			}
		}
	}
	return ctx
}

func (q *memberStorageQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Both the real Store and Service share this exact one-connection pool. Merely
// limiting the Service pool while the Store uses another would miss a held-auth
// transaction deadlock. Other administration fixture helpers remain reusable.
func memberStorageSingleConnectionFixture(t *testing.T, tracer *memberStorageQueryTracer) *fixture {
	t.Helper()
	ctx := context.Background()
	db := singleConnectionDatabase(t, ctx, tracer)
	check(t, receiptstore.Migrate(ctx, db))
	check(t, Migrate(ctx, db))
	var members [2]*receiptstore.LocalMember
	for i := range members {
		member, idErr := mailID(newID(t))
		check(t, idErr)
		m, e := receiptstore.OpenLocalMember(
			ctx,
			db,
			member,
			fmt.Sprint("operator-site-", i),
			t.TempDir(),
			hotstore.Options{MaxMessageBytes: 1 << 20, MaxBytes: 16 << 20, MaxConcurrentAppends: 4},
		)
		check(t, e)
		members[i] = m
		t.Cleanup(func() {
			if e := m.Close(); e != nil {
				t.Error(e)
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
	service, err := New(db, mail, time.Hour)
	check(t, err)
	seedFixtureOperator(t, service)
	root, err := service.Login(ctx, "operator", "test-operator-secret")
	check(t, err)
	f := &fixture{service, db, mail, root, newID(t), newID(t), newID(t)}
	check(t, service.PutOrganization(ctx, root.Token, Organization{f.org, "First"}))
	check(t, service.PutOrganization(ctx, root.Token, Organization{f.other, "Second"}))
	return f
}

func memberStorageFixtureMember(t *testing.T, f *fixture) string {
	t.Helper()
	var member string
	check(t, f.db.QueryRow(context.Background(), `SELECT id FROM fry_member ORDER BY id LIMIT 1`).Scan(&member))
	return member
}

func emptyMemberStorage(t *testing.T, snapshot receiptstore.MemberStorageSnapshot) {
	t.Helper()
	if !reflect.DeepEqual(snapshot, receiptstore.MemberStorageSnapshot{}) {
		t.Fatal("failed authorization exposed partial diagnostics")
	}
}

func TestMemberStorageGlobalOnlyAndSingleConnectionCheckout(t *testing.T) {
	tracer := &memberStorageQueryTracer{}
	f := memberStorageSingleConnectionFixture(t, tracer)
	ctx := context.Background()
	member := memberStorageFixtureMember(t, f)
	manager := f.manager(t, true)
	check(
		t,
		f.service.ReplaceGrants(
			ctx,
			f.root.Token,
			manager.Identity.ID,
			[]Grant{{Scope: f.org, Content: true}, {Scope: f.other, Content: true}},
		),
	)
	// Configuration permission suffices: do not silently require message-content
	// permission for an installation operator inspecting bounded storage metadata.
	operator, err := f.service.PutUser(ctx, f.root.Token, "diagnostics", "test-diagnostics-secret", true)
	check(t, err)
	check(t, f.service.ReplaceGrants(ctx, f.root.Token, operator, []Grant{{Scope: "*", Content: false}}))
	login, err := f.service.Login(ctx, "diagnostics", "test-diagnostics-secret")
	check(t, err)
	// Bound the inspections, not password hashing in fixture setup. A retained
	// authorization checkout must still exhaust this one-connection pool's budget.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tracer.armed.Store(true)
	for _, token := range []string{"", manager.Token} {
		snapshot, err := f.service.MemberStorage(ctx, token, member, "", 64)
		want := ErrForbidden
		if token == "" {
			want = ErrUnauthorized
		}
		requireErrorIs(t, err, want)
		emptyMemberStorage(t, snapshot)
	}
	if tracer.calls.Load() != 0 {
		t.Fatal("unauthorized diagnostics entered storage authority before permission check")
	}
	snapshot, err := f.service.MemberStorage(ctx, login.Token, member, "", 1)
	check(t, err)
	if snapshot.Member != member || snapshot.Current.FileID == "" || len(snapshot.Files) != 1 ||
		snapshot.Runtime.Status != "sampled" {
		t.Fatal("real configured member snapshot missing")
	}
	if tracer.calls.Load() != 1 || f.db.Stat().AcquiredConns() != 0 {
		t.Fatal("authority checkout or release unexpected", tracer.calls.Load(), f.db.Stat().AcquiredConns())
	}
}

func TestMemberStorageRevocationDuringInspectionReturnsNoSnapshot(t *testing.T) {
	tracer := &memberStorageQueryTracer{pause: true, entered: make(chan struct{}), resume: make(chan struct{})}
	f := memberStorageSingleConnectionFixture(t, tracer)
	member := memberStorageFixtureMember(t, f)
	cfg := f.db.Config()
	cfg.ConnConfig.Tracer = nil
	control, err := pgxpool.NewWithConfig(context.Background(), cfg)
	check(t, err)
	t.Cleanup(control.Close)
	revoker, err := New(control, f.mail, time.Hour)
	check(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tracer.armed.Store(true)
	type result struct {
		snapshot receiptstore.MemberStorageSnapshot
		err      error
	}
	done := make(chan result, 1)
	go func() {
		snapshot, e := f.service.MemberStorage(ctx, f.root.Token, member, "", 64)
		done <- result{snapshot, e}
	}()
	select {
	case <-tracer.entered:
	case <-ctx.Done():
		close(tracer.resume)
		<-done
		t.Fatal("core inspection never acquired the single connection; auth transaction may still be held")
	}
	// Revoke through the actual service on another connection while the storage
	// query owns the sole inspection connection. No permission row is fabricated.
	err = revoker.Logout(ctx, f.root.Token)
	close(tracer.resume)
	check(t, err)
	var got result
	select {
	case got = <-done:
	case <-ctx.Done():
		t.Fatal("inspection did not join after revocation")
	}
	if !errors.Is(got.err, ErrUnauthorized) {
		t.Fatal("revoked session received storage snapshot", got.err)
	}
	emptyMemberStorage(t, got.snapshot)
}

func singleConnectionDatabase(t *testing.T, ctx context.Context, tracer *memberStorageQueryTracer) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("FRY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("run scripts/tests/receipts.sh for PostgreSQL-backed administration tests")
	}
	control, err := pgxpool.New(ctx, dsn)
	check(t, err)
	t.Cleanup(control.Close)
	schema := "fry_member_admin_" + newID(t)
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = control.Exec(ctx, "CREATE SCHEMA "+quoted)
	check(t, err)
	t.Cleanup(func() {
		if _, e := control.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); e != nil {
			t.Error(e)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	check(t, err)
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.Tracer = tracer
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	check(t, err)
	t.Cleanup(db.Close)
	return db
}
