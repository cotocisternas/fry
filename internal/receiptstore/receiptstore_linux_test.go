//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	s       *Store
	pool    *pgxpool.Pool
	members [2]*LocalMember
	dirs    [2]string
	org     ID
}

func newID(t *testing.T) ID {
	t.Helper()
	id, err := hotstore.NewID()
	requireReceiptNoError(t, err)
	return id
}

func hotOptions() hotstore.Options {
	return hotstore.Options{MaxMessageBytes: 4 << 20, MaxBytes: 128 << 20, MaxConcurrentAppends: 8}
}

func setup(t *testing.T) *fixture {
	t.Helper()
	return setupWithPoolConfig(
		t,
		nil,
		hotOptions(),
		Options{Lease: 10 * time.Second, RecoveryRetention: 24 * time.Hour},
	)
}

// setupWithPoolConfig changes only opt-in test fixtures; setup retains the
// ordinary pool, append and lease defaults used by the correctness suite.
func setupWithPoolConfig(t *testing.T, configure func(*pgxpool.Config), hot hotstore.Options, opts Options) *fixture {
	t.Helper()
	dsn := os.Getenv("FRY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set FRY_TEST_DATABASE_URL or run scripts/tests/receipts.sh for real PostgreSQL tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	requireReceiptNoError(t, err)
	t.Cleanup(admin.Close)
	schema := "fry_test_" + newID(t).String()
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
	requireReceiptNoError(t, err)
	t.Cleanup(func() {
		if _, e := admin.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); e != nil {
			t.Error(e)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	requireReceiptNoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.RuntimeParams["application_name"] = schema
	cfg.MaxConns = 8
	if configure != nil {
		configure(cfg)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	requireReceiptNoError(t, err)
	t.Cleanup(pool.Close)
	err = Migrate(ctx, pool)
	requireReceiptNoError(t, err)
	f := &fixture{pool: pool, org: newID(t)}
	for i := range f.members {
		f.dirs[i] = t.TempDir()
		member, e := OpenLocalMember(
			context.Background(),
			pool,
			newID(t),
			[]string{"zone-a", "zone-b"}[i],
			f.dirs[i],
			hot,
		)
		requireReceiptNoError(t, e)
		t.Cleanup(func() {
			if err := member.Close(); err != nil {
				t.Error(err)
			}
		})
		f.members[i] = member
	}
	f.s, err = New(ctx, pool, f.members, opts)
	requireReceiptNoError(t, err)
	return f
}

func (f *fixture) envelope(t *testing.T) Envelope {
	t.Helper()
	return Envelope{
		Organization: f.org,
		MailFrom:     "partner@example.test",
		Destinations: []Destination{{Inbox: newID(t), Recipients: []string{"invoices@example.test"}}},
	}
}

func accept(t *testing.T, s *Store, attempt ID, e Envelope, body string) Receipt {
	t.Helper()
	r, err := s.Accept(context.Background(), attempt, e, io.NopCloser(strings.NewReader(body)))
	requireReceiptNoError(t, err)
	return r
}

func prepare(t *testing.T, f *fixture, attempt ID, e Envelope) claim {
	t.Helper()
	e, b, err := normalize(e)
	requireReceiptNoError(t, err)
	c, r, err := f.s.db.prepare(context.Background(), attempt, e, b, f.members)
	requireReceiptConditionf(t, err == nil, "prepare: %v %+v", err, r)
	requireReceiptConditionf(t, r == nil, "prepare: %v %+v", err, r)
	return c
}

func scalar(t *testing.T, p *pgxpool.Pool, q string, args ...any) int64 {
	t.Helper()
	return scalarContext(t.Context(), t, p, q, args...)
}

func scalarContext(ctx context.Context, t *testing.T, p *pgxpool.Pool, q string, args ...any) int64 {
	t.Helper()
	var n int64
	requireReceiptNoError(t, p.QueryRow(ctx, q, args...).Scan(&n))
	return n
}

func execSQL(t *testing.T, p *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	{
		_, err := p.Exec(context.Background(), q, args...)
		requireReceiptCondition(t, err == nil, err)
	}
}

func expire(t *testing.T, f *fixture, attempt ID) {
	t.Helper()
	execSQL(
		t,
		f.pool,
		`UPDATE fry_attempt SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`,
		attempt.String(),
	)
}

func status(t *testing.T, f *fixture, attempt ID) Status {
	t.Helper()
	st, err := f.s.Status(context.Background(), f.org, attempt)
	requireReceiptNoError(t, err)
	return st
}

func copies(t *testing.T, f *fixture, c claim, body string) {
	t.Helper()
	for _, m := range f.members {
		requireReceiptNoError(t, m.write(context.Background(), f.s.db, c, io.NopCloser(strings.NewReader(body))))
	}
}

func TestAcceptPublishesAllStateAtomically(t *testing.T) {
	f := setup(t)
	e := f.envelope(t)
	inbox := e.Destinations[0].Inbox
	e.Destinations = append(
		e.Destinations,
		Destination{Inbox: inbox, Recipients: []string{"billing@example.test", "invoices@example.test"}},
		Destination{Inbox: newID(t), Recipients: []string{"archive@example.test"}},
	)
	e.Source = &Source{Connection: newID(t), Folder: "Invoices", UIDValidity: 12, UID: 34}
	attempt := newID(t)
	body := "Subject: raw\r\n\r\n\x00untouched\r\n"
	receipt := accept(t, f.s, attempt, e, body)
	requireReceiptCondition(t, receipt.Attempt == attempt, "wrong receipt", receipt)
	requireReceiptCondition(t, receipt.Organization == f.org, "wrong receipt", receipt)
	requireReceiptCondition(t, receipt.Size == int64(len(body)), "wrong receipt", receipt)
	requireReceiptCondition(t, receipt.SHA256 == sha256.Sum256([]byte(body)), "wrong receipt", receipt)
	requireAcceptedReceiptAuthority(t, f)
	var output bytes.Buffer
	{
		err := f.s.Read(context.Background(), f.org, receipt.ID, 0, receipt.Size, &output)
		requireReceiptCondition(t, err == nil, "cannot read accepted bytes", err)
		requireReceiptCondition(t, output.String() == body, "cannot read accepted bytes", err)
	}
	{
		st := status(t, f, attempt)
		requireReceiptCondition(t, st.State == "accepted", st)
		requireReceiptCondition(t, st.Receipt != nil, st)
		requireReceiptCondition(t, st.Receipt.ID == receipt.ID, st)
	}
	{
		_, err := f.s.Status(context.Background(), newID(t), attempt)
		requireReceiptCondition(t, errors.Is(err, ErrNotFound), "cross-organization status exposed", err)
	}
	{
		err := f.s.Read(context.Background(), newID(t), receipt.ID, 0, 1, io.Discard)
		requireReceiptCondition(t, errors.Is(err, ErrNotFound), "cross-organization bytes exposed", err)
	}
	{
		err := f.s.Abort(context.Background(), f.org, attempt)
		requireReceiptCondition(t, errors.Is(err, ErrAccepted), "accepted receipt aborted", err)
	}
	// A different attempt cannot publish the same collected source message again.
	{
		_, err := f.s.Accept(context.Background(), newID(t), e, io.NopCloser(strings.NewReader(body)))
		requireReceiptCondition(t, errors.Is(err, ErrSourceConflict), "source identity duplicated", err)
	}
}

func TestNoAcceptanceBeforeTwoDurableCopies(t *testing.T) {
	f := setup(t)
	attempt := newID(t)
	e := f.envelope(t)
	c := prepare(t, f, attempt, e)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_target") == 2,
		"targets were not preregistered",
	)
	requireReceiptCondition(t, len(f.members[0].bytes.Inventory()) == 0, "targets were not preregistered")
	requireReceiptCondition(t, len(f.members[1].bytes.Inventory()) == 0, "targets were not preregistered")
	requireReceiptNoError(
		t,
		f.members[0].write(context.Background(), f.s.db, c, io.NopCloser(strings.NewReader("body"))),
	)
	{
		_, err := f.s.db.finalize(context.Background(), c)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "single copy accepted", err)
	}
	requireReceiptCondition(t, scalar(t, f.pool, "SELECT count(*) FROM fry_receipt") == 0, "premature visibility")
	requireReceiptCondition(t, scalar(t, f.pool, "SELECT count(*) FROM fry_work") == 0, "premature visibility")
	requireReceiptNoError(
		t,
		f.members[1].write(context.Background(), f.s.db, c, io.NopCloser(strings.NewReader("different"))),
	)
	{
		_, err := f.s.db.finalize(context.Background(), c)
		requireReceiptCondition(t, errors.Is(err, ErrConflict), "mismatched replica bytes accepted", err)
	}
}

func TestMemberFailureAndTerminalAbort(t *testing.T) {
	f := setup(t)
	attempt := newID(t)
	e := f.envelope(t)
	if err := f.members[1].bytes.Close(); err != nil {
		t.Error(err)
	}
	{
		r, err := f.s.Accept(context.Background(), attempt, e, io.NopCloser(strings.NewReader("body")))
		requireReceiptCondition(t, err != nil, "failed member accepted", r, err)
		requireReceiptCondition(t, r == (Receipt{}), "failed member accepted", r, err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_receipt") == 0,
		"failure lost target obligations or published receipt",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_target") == 2,
		"failure lost target obligations or published receipt",
	)
	requireReceiptNoError(t, f.s.Abort(context.Background(), f.org, attempt))
	requireReceiptCondition(t, status(t, f, attempt).State == "aborted", "incorrect cleanup state")
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_cleanup WHERE cancel_confirmed AND NOT bytes_removed") == 2,
		"incorrect cleanup state",
	)
	{
		_, err := f.s.Accept(context.Background(), attempt, e, io.NopCloser(strings.NewReader("body")))
		requireReceiptCondition(t, errors.Is(err, ErrAborted), "aborted attempt reused", err)
	}
}

func TestCopiesWithoutReceiptRecoverAfterLeaseTakeover(t *testing.T) {
	f := setup(t)
	attempt := newID(t)
	e := f.envelope(t)
	f.s.afterCopies = func() error { return io.ErrUnexpectedEOF }
	{
		_, err := f.s.Accept(context.Background(), attempt, e, io.NopCloser(strings.NewReader("body")))
		requireReceiptCondition(t, errors.Is(err, io.ErrUnexpectedEOF), err)
	}
	requireReceiptCondition(t, scalar(t, f.pool, "SELECT count(*) FROM fry_receipt") == 0, "incorrect precommit state")
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_target WHERE locator IS NOT NULL") == 2,
		"incorrect precommit state",
	)
	f.s.afterCopies = nil
	expire(t, f, attempt)
	r := accept(t, f.s, attempt, e, "body")
	requireReceiptCondition(t, status(t, f, attempt).Epoch == 2, "takeover failed")
	requireReceiptCondition(t, r.Size == 4, "takeover failed")
	for _, m := range f.members {
		requireReceiptCondition(t, len(m.bytes.Inventory()) == 1, "takeover duplicated byte records")
	}
}

type unreadSource struct {
	reads    int
	closed   bool
	closeErr error
}

func (s *unreadSource) Read([]byte) (int, error) { s.reads++; return 0, io.ErrUnexpectedEOF }
func (s *unreadSource) Close() error             { s.closed = true; return s.closeErr }
func TestLostCommitReplyResolvesThroughStatus(t *testing.T) {
	f := setup(t)
	attempt := newID(t)
	e := f.envelope(t)
	f.s.db.afterCommit = func() error { return io.ErrUnexpectedEOF }
	{
		r, err := f.s.Accept(context.Background(), attempt, e, io.NopCloser(strings.NewReader("body")))
		requireReceiptCondition(t, errors.Is(err, ErrUncertain), "lost reply treated as success", r, err)
		requireReceiptCondition(t, r == (Receipt{}), "lost reply treated as success", r, err)
	}
	st := status(t, f, attempt)
	requireReceiptCondition(t, st.State == "accepted", "committed receipt missing", st)
	requireReceiptCondition(t, st.Receipt != nil, "committed receipt missing", st)
	f.s.db.afterCommit = nil
	source := &unreadSource{}
	r, err := f.s.Accept(context.Background(), attempt, e, source)
	requireReceiptCondition(t, err == nil, "retry did not resolve stable receipt", r, err)
	requireReceiptCondition(t, r.ID == st.Receipt.ID, "retry did not resolve stable receipt", r, err)
	requireReceiptCondition(
		t,
		r.ReceivedAt.Equal(st.Receipt.ReceivedAt),
		"retry did not resolve stable receipt",
		r,
		err,
	)
	requireReceiptCondition(t, source.reads == 0, "retry did not resolve stable receipt", r, err)
	requireReceiptCondition(t, source.closed, "retry did not resolve stable receipt", r, err)
	closingSource := &unreadSource{closeErr: errors.New("synthetic source close failure")}
	again, closeErr := f.s.Accept(t.Context(), attempt, e, closingSource)
	requireReceiptCondition(t, closeErr == nil, "source cleanup changed accepted receipt", closeErr)
	requireReceiptCondition(t, again == r, "source cleanup changed accepted receipt", closeErr)
	requireReceiptCondition(t, closingSource.reads == 0, "source cleanup changed accepted receipt", closeErr)
	requireReceiptCondition(t, closingSource.closed, "source cleanup changed accepted receipt", closeErr)
	requireReceiptCondition(t, scalar(t, f.pool, "SELECT count(*) FROM fry_work") == 1, "retry duplicated work")
	changed := e
	changed.MailFrom = "different@example.test"
	{
		_, err = f.s.Accept(context.Background(), attempt, changed, io.NopCloser(strings.NewReader("body")))
		requireReceiptCondition(t, errors.Is(err, ErrConflict), "identity metadata changed", err)
	}
}

func TestStaleEpochAndProfileCannotPublish(t *testing.T) {
	f := setup(t)
	attempt := newID(t)
	e := f.envelope(t)
	old := prepare(t, f, attempt, e)
	copies(t, f, old, "body")
	expire(t, f, attempt)
	current := prepare(t, f, attempt, e)
	requireReceiptCondition(t, current.Epoch == old.Epoch+1, "epoch not advanced")
	loc := f.members[0].bytes.Inventory()[0]
	{
		err := f.s.db.recordCopy(context.Background(), old, f.members[0], loc)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "stale member report accepted", err)
	}
	{
		_, err := f.s.db.finalize(context.Background(), old)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "stale generation committed", err)
	}
	execSQL(t, f.pool, "UPDATE fry_profile SET epoch=epoch+1")
	{
		_, err := f.s.db.finalize(context.Background(), current)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "profile change ignored", err)
	}
	requireReceiptCondition(t, scalar(t, f.pool, "SELECT count(*) FROM fry_receipt") == 0, "stale receipt visible")
}

func TestAbortRejectsLateCopiesAndNewOperations(t *testing.T) {
	f := setup(t)
	attempt := newID(t)
	c := prepare(t, f, attempt, f.envelope(t))
	copies(t, f, c, "body")
	requireReceiptNoError(t, f.s.Abort(context.Background(), f.org, attempt))
	{
		err := f.s.db.recordCopy(context.Background(), c, f.members[0], f.members[0].bytes.Inventory()[0])
		requireReceiptCondition(t, errors.Is(err, ErrAborted), "late copy published", err)
	}
	{
		err := f.members[0].write(context.Background(), f.s.db, c, io.NopCloser(strings.NewReader("body")))
		requireReceiptCondition(t, errors.Is(err, ErrAborted), "queued writer entered", err)
	}
	{
		_, err := f.s.db.finalize(context.Background(), c)
		requireReceiptCondition(t, errors.Is(err, ErrAborted), "abort lost race", err)
	}
	// Recreate the local operation registry: terminal DB state still fences writes.
	if err := f.members[0].Close(); err != nil {
		t.Error(err)
	}
	restarted, openErr := OpenLocalMember(
		context.Background(),
		f.pool,
		f.members[0].id,
		f.members[0].domain,
		f.dirs[0],
		hotOptions(),
	)
	requireReceiptNoError(t, openErr)
	defer func() {
		if err := restarted.Close(); err != nil {
			t.Error(err)
		}
	}()
	{
		err := restarted.write(context.Background(), f.s.db, c, io.NopCloser(strings.NewReader("body")))
		requireReceiptCondition(t, errors.Is(err, ErrAborted), "restart forgot abortion", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_cleanup WHERE bytes_removed") == 0,
		"cancel falsely claimed erasure",
	)
}

func TestDatabaseDisconnectBeforeCommitDoesNotAcknowledge(t *testing.T) {
	f := setup(t)
	attempt := newID(t)
	e := f.envelope(t)
	f.s.db.beforeCommit = func(tx pgx.Tx) error {
		_, err := f.pool.Exec(context.Background(), "SELECT pg_terminate_backend($1)", tx.Conn().PgConn().PID())
		return err
	}
	{
		r, err := f.s.Accept(context.Background(), attempt, e, io.NopCloser(strings.NewReader("body")))
		requireReceiptCondition(t, err != nil, "disconnected commit acknowledged", r, err)
		requireReceiptCondition(t, r == (Receipt{}), "disconnected commit acknowledged", r, err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_receipt") == 0,
		"partial transaction escaped",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_reference") == 0,
		"partial transaction escaped",
	)
	requireReceiptCondition(t, scalar(t, f.pool, "SELECT count(*) FROM fry_work") == 0, "partial transaction escaped")
	f.s.db.beforeCommit = nil
	expire(t, f, attempt)
	accept(t, f.s, attempt, e, "body")
}

func TestSurvivingMemberAndRestartCanReadReceipt(t *testing.T) {
	f := setup(t)
	attempt := newID(t)
	e := f.envelope(t)
	r := accept(t, f.s, attempt, e, "durable bytes")
	for i, m := range f.members {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
		reopened, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, f.dirs[i], hotOptions())
		requireReceiptNoError(t, err)
		t.Cleanup(func() {
			if err := reopened.Close(); err != nil {
				t.Error(err)
			}
		})
		f.members[i] = reopened
	}
	s, err := New(context.Background(), f.pool, f.members, f.s.db.opts)
	requireReceiptNoError(t, err)
	f.s = s
	// Force the first attempted locator unavailable, then verify fallback.
	first := 0
	if f.members[1].id.String() < f.members[0].id.String() {
		first = 1
	}
	if err := f.members[first].bytes.Close(); err != nil {
		t.Error(err)
	}
	var output bytes.Buffer
	{
		err = s.Read(context.Background(), f.org, r.ID, 0, r.Size, &output)
		requireReceiptCondition(t, err == nil, "surviving copy failed", err)
		requireReceiptCondition(t, output.String() == "durable bytes", "surviving copy failed", err)
	}
	st := status(t, f, attempt)
	requireReceiptCondition(t, st.Receipt != nil, "restart forgot receipt")
	requireReceiptCondition(t, st.Receipt.ID == r.ID, "restart forgot receipt")
}

type gatedSource struct {
	started, release, closed chan struct{}
	startOnce, closeOnce     sync.Once
	reader                   *strings.Reader
}

func newGated() *gatedSource {
	return &gatedSource{
		started: make(chan struct{}),
		release: make(chan struct{}),
		closed:  make(chan struct{}),
		reader:  strings.NewReader("body"),
	}
}

func (g *gatedSource) Read(b []byte) (int, error) {
	g.startOnce.Do(func() { close(g.started) })
	select {
	case <-g.release:
		return g.reader.Read(b)
	case <-g.closed:
		return 0, io.ErrClosedPipe
	}
}
func (g *gatedSource) Close() error { g.closeOnce.Do(func() { close(g.closed) }); return nil }
func wait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestAbortCancelsActiveStreams(t *testing.T) {
	f := setup(t)
	attempt := newID(t)
	e := f.envelope(t)
	source := newGated()
	defer func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	}()
	done := make(chan error, 1)
	go func() { _, err := f.s.Accept(context.Background(), attempt, e, source); done <- err }()
	wait(t, source.started)
	{
		_, err := f.s.Accept(context.Background(), attempt, e, io.NopCloser(strings.NewReader("overlap")))
		requireReceiptCondition(t, errors.Is(err, ErrBusy), "overlapping attempt entered", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	requireReceiptNoError(t, f.s.Abort(ctx, f.org, attempt))
	select {
	case err := <-done:
		requireReceiptCondition(t, err != nil, "aborted accept succeeded")
	case <-ctx.Done():
		t.Fatal("source not cancelled")
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_cleanup WHERE cancel_confirmed AND NOT bytes_removed") == 2,
		"missing quiescence",
	)
}

func TestLeaseRenewalAndExpiryAfterLockWait(t *testing.T) {
	t.Run("renewal", func(t *testing.T) {
		f := setup(t)
		f.s.db.opts.Lease = 600 * time.Millisecond
		attempt := newID(t)
		e := f.envelope(t)
		source := newGated()
		defer func() {
			if err := source.Close(); err != nil {
				t.Error(err)
			}
		}()
		done := make(chan error, 1)
		go func() { _, err := f.s.Accept(context.Background(), attempt, e, source); done <- err }()
		wait(t, source.started)
		time.Sleep(900 * time.Millisecond)
		close(source.release)
		select {
		case err := <-done:
			requireReceiptCondition(t, err == nil, "lease not renewed", err)
		case <-time.After(5 * time.Second):
			t.Fatal("renewal stalled")
		}
	})
	t.Run("deadline-after-lock", func(t *testing.T) {
		f := setup(t)
		attempt := newID(t)
		c := prepare(t, f, attempt, f.envelope(t))
		copies(t, f, c, "body")
		tx, err := f.pool.Begin(context.Background())
		requireReceiptNoError(t, err)
		defer rollback(context.Background(), tx)
		_, err = tx.Exec(context.Background(), "SELECT id FROM fry_attempt WHERE id=$1 FOR UPDATE", attempt.String())
		requireReceiptNoError(t, err)
		// Set a deadline that is still live when the finalizer's transaction
		// begins, then hold the lock until it expires. A transaction-start
		// timestamp would incorrectly permit this publication.
		_, err = tx.Exec(
			context.Background(),
			"UPDATE fry_attempt SET lease_until=clock_timestamp()+interval '500 milliseconds' "+
				"WHERE id=$1",
			attempt.String(),
		)
		requireReceiptNoError(t, err)
		done := make(chan error, 1)
		go func() { _, e := f.s.db.finalize(context.Background(), c); done <- e }()
		deadline := time.Now().Add(3 * time.Second)
		for scalar(t, f.pool, "SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND "+
			"wait_event_type='Lock'", f.pool.Config().ConnConfig.RuntimeParams["application_name"]) == 0 {
			requireReceiptCondition(t, !time.Now().After(deadline), "finalizer never waited for its guard lock")
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(600 * time.Millisecond)
		err = tx.Commit(context.Background())
		requireReceiptNoError(t, err)
		select {
		case err := <-done:
			requireReceiptCondition(t, errors.Is(err, ErrStale), "expired locked claim accepted", err)
		case <-time.After(5 * time.Second):
			t.Fatal("claim lock stalled")
		}
	})
}

func TestAbortAndFinalizeSerialize(t *testing.T) {
	f := setup(t)
	for range 8 {
		attempt := newID(t)
		c := prepare(t, f, attempt, f.envelope(t))
		copies(t, f, c, "body")
		start := make(chan struct{})
		var acceptErr, abortErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, acceptErr = f.s.db.finalize(context.Background(), c) }()
		go func() { defer wg.Done(); <-start; abortErr = f.s.Abort(context.Background(), f.org, attempt) }()
		close(start)
		wg.Wait()
		st := status(t, f, attempt)
		switch st.State {
		case "accepted":
			requireReceiptCondition(t, acceptErr == nil, "inconsistent acceptance", acceptErr, abortErr)
			requireReceiptCondition(t, errors.Is(abortErr, ErrAccepted), "inconsistent acceptance", acceptErr, abortErr)
		case "aborted":
			requireReceiptCondition(t, abortErr == nil, "inconsistent abort", acceptErr, abortErr)
			requireReceiptCondition(t, errors.Is(acceptErr, ErrAborted), "inconsistent abort", acceptErr, abortErr)
		default:
			t.Fatal("nonterminal outcome", st)
		}
	}
}

func TestRejectFakePlacement(t *testing.T) {
	f := setup(t)
	sameDomain, err := OpenLocalMember(
		context.Background(),
		f.pool,
		newID(t),
		f.members[0].domain,
		t.TempDir(),
		hotOptions(),
	)
	requireReceiptNoError(t, err)
	defer func() {
		if err := sameDomain.Close(); err != nil {
			t.Error(err)
		}
	}()
	{
		_, err := New(context.Background(), f.pool, [2]*LocalMember{f.members[0], sameDomain}, f.s.db.opts)
		requireReceiptCondition(t, err != nil, "same-domain placement accepted")
	}
	// The file catalog can reject a new identity before taking the existing
	// root's lock. Either refusal must leave the original registry authoritative.
	duplicateID := newID(t)
	if duplicate, err := OpenLocalMember(
		context.Background(),
		f.pool,
		duplicateID,
		"zone-c",
		f.dirs[0],
		hotOptions(),
	); duplicate != nil ||
		(!errors.Is(err, hotstore.ErrBusy) && !errors.Is(err, ErrConflict)) {
		if duplicate != nil {
			if err := duplicate.Close(); err != nil {
				t.Error(err)
			}
		}
		t.Fatal("duplicate cancellation registry allowed", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member WHERE id=$1`, duplicateID.String()) == 0,
		"refused duplicate identity was registered",
	)
	accept(t, f.s, newID(t), f.envelope(t), "original member remains usable")
}

func TestParallelStreamingReceipts(t *testing.T) {
	f := setup(t)
	body := bytes.Repeat([]byte("raw\x00email\r\n"), 180000)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	receipts := make(chan Receipt, 4)
	for range 4 {
		attempt, e := newID(t), f.envelope(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := f.s.Accept(context.Background(), attempt, e, io.NopCloser(bytes.NewReader(body)))
			errs <- err
			receipts <- r
		}()
	}
	wg.Wait()
	close(errs)
	close(receipts)
	for err := range errs {
		requireReceiptNoError(t, err)
	}
	for r := range receipts {
		var output bytes.Buffer
		{
			err := f.s.Read(context.Background(), f.org, r.ID, 0, r.Size, &output)
			requireReceiptCondition(t, err == nil, "parallel stream mismatch", err)
			requireReceiptCondition(t, bytes.Equal(output.Bytes(), body), "parallel stream mismatch", err)
		}
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_receipt") == 4,
		"missing concurrent receipt state",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_work") == 4,
		"missing concurrent receipt state",
	)
}

func TestTransactionOverridesAsyncSessionSetting(t *testing.T) {
	f := setup(t)
	cfg := f.pool.Config()
	cfg.ConnConfig.RuntimeParams["synchronous_commit"] = "off"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	requireReceiptNoError(t, err)
	defer pool.Close()
	var setting string
	{
		err = pool.QueryRow(context.Background(), "SHOW synchronous_commit").Scan(&setting)
		requireReceiptCondition(t, err == nil, "test session not async", setting, err)
		requireReceiptCondition(t, setting == "off", "test session not async", setting, err)
	}
	var members [2]*LocalMember
	for i, m := range f.members {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
		member, err := OpenLocalMember(context.Background(), pool, m.id, m.domain, f.dirs[i], hotOptions())
		requireReceiptNoError(t, err)
		defer func() {
			if err := member.Close(); err != nil {
				t.Error(err)
			}
		}()
		members[i] = member
	}
	s, err := New(context.Background(), pool, members, f.s.db.opts)
	requireReceiptNoError(t, err)
	s.db.beforeCommit = func(tx pgx.Tx) error {
		var setting string
		err := tx.QueryRow(context.Background(), "SHOW synchronous_commit").Scan(&setting)
		if err != nil {
			return err
		}
		if setting != "on" {
			t.Errorf("receipt commit is %s", setting)
		}
		return nil
	}
	accept(t, s, newID(t), f.envelope(t), "body")
}

func TestLeaseExpiresDuringPublication(t *testing.T) {
	f := setup(t)
	f.s.db.opts.Lease = 300 * time.Millisecond
	attempt := newID(t)
	c := prepare(t, f, attempt, f.envelope(t))
	copies(t, f, c, "body")
	f.s.db.beforeCommit = func(pgx.Tx) error { time.Sleep(350 * time.Millisecond); return nil }
	{
		_, err := f.s.db.finalize(context.Background(), c)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "publication ignored expired lease", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_receipt") == 0,
		"expired transaction escaped",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_reference") == 0,
		"expired transaction escaped",
	)
}

func TestSourceReservationReleasedOnlyOnAbort(t *testing.T) {
	f := setup(t)
	attempt := newID(t)
	e := f.envelope(t)
	e.Source = &Source{Connection: newID(t), Folder: "INBOX", UIDValidity: 1, UID: 100}
	prepare(t, f, attempt, e)
	{
		_, err := f.s.Accept(context.Background(), newID(t), e, io.NopCloser(strings.NewReader("body")))
		requireReceiptCondition(t, errors.Is(err, ErrSourceConflict), "pending source reservation ignored", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_source WHERE accepted") == 0,
		"source checkpoint advanced without acceptance",
	)
	requireReceiptNoError(t, f.s.Abort(context.Background(), f.org, attempt))
	accept(t, f.s, newID(t), e, "body")
	// An earlier UID still has independent progress; a later UID did not skip it.
	earlier := *e.Source
	earlier.UID = 99
	e.Source = &earlier
	accept(t, f.s, newID(t), e, "earlier")
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_source WHERE accepted") == 2,
		"out-of-order source acceptance skipped a message",
	)
}

// encodedTestBytes is used only with validated, fixed fixture size policies.
func encodedTestBytes(size int64) int64 {
	n, err := hotstore.EncodedRecordBytes(size)
	if err != nil {
		panic(err)
	}
	return n
}

func receiptTestValue[T any](t testing.TB, value any) T {
	t.Helper()
	if value == nil {
		var zero T
		return zero
	}
	result, ok := value.(T)
	requireReceiptConditionf(t, ok, "unexpected fixture value type %T", value)
	return result
}

func storageReadFile(root, name string) (data []byte, resultErr error) {
	resultErr = withStorageRoot(root, func(r *os.Root) error {
		file, err := r.Open(name)
		if err != nil {
			return err
		}
		data, err = io.ReadAll(file)
		return errors.Join(err, file.Close())
	})
	return data, resultErr
}

// splitStorageFile keeps test fixture corruption opens anchored to their parent.
func splitStorageFile(path string, flags int, mode fs.FileMode) (*os.File, error) {
	return openStorageFile(filepath.Dir(path), filepath.Base(path), flags, mode)
}

func requireAcceptedReceiptAuthority(t *testing.T, f *fixture) {
	t.Helper()
	for table, want := range map[string]int64{
		"fry_receipt":     1,
		"fry_target":      2,
		"fry_destination": 2,
		"fry_reference":   3,
		"fry_work":        2,
	} {
		{
			n := scalar(t, f.pool, "SELECT count(*) FROM "+table)
			requireReceiptConditionf(t, n == want, "%s: got %d, want %d", table, n, want)
		}
	}
	{
		n := scalar(t, f.pool, "SELECT count(*) FROM fry_target WHERE locator IS NOT NULL")
		requireReceiptCondition(t, n == 2, "missing durable proof")
	}
	{
		n := scalar(t, f.pool, "SELECT count(*) FROM fry_source WHERE accepted")
		requireReceiptCondition(t, n == 1, "source checkpoint not accepted")
	}
	{
		n := scalar(
			t,
			f.pool,
			`SELECT extract(epoch FROM (expires_at-r.received_at))::bigint FROM fry_reference f `+
				`JOIN fry_receipt r ON r.id=f.receipt WHERE f.kind='recovery'`,
		)
		requireReceiptCondition(t, n == 86400, "wrong retention clock", n)
	}
}
