package smtpserver

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	mailsmtp "net/smtp"
	"net/textproto"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/emersion/go-smtp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Existing setup freezes legacy member profiles. Construct the same SMTP
// fixture with bounded profiles from the start, keeping real acceptance and
// the existing TCP/client helpers rather than replacing Store.Accept.
func setupPrereadCapacity(t *testing.T) (*fixture, [2]receiptstore.ID) {
	t.Helper()
	dsn := os.Getenv("FRY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("run scripts/tests/receipts.sh for PostgreSQL-backed SMTP tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	requireTestCondition(t, err == nil, err)
	t.Cleanup(admin.Close)
	schema := "fry_smtp_preread_" + id(t).String()
	quoted := pgx.Identifier{schema}.Sanitize()
	{
		_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
		requireTestCondition(t, err == nil, err)
	}
	t.Cleanup(func() {
		if _, e := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); e != nil {
			t.Error(e)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	requireTestCondition(t, err == nil, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	requireTestCondition(t, err == nil, err)
	t.Cleanup(pool.Close)
	{
		err = receiptstore.Migrate(ctx, pool)
		requireTestCondition(t, err == nil, err)
	}
	f := &fixture{pool: pool, org: id(t), other: id(t)}
	const maxMessage = 128 << 10
	memberIDs := openPrereadMembers(t, f, ctx, pool, maxMessage)
	f.store, err = receiptstore.New(
		ctx,
		pool,
		f.members,
		receiptstore.Options{Lease: time.Second, RecoveryRetention: time.Hour},
	)
	requireTestCondition(t, err == nil, err)
	f.config = Config{
		Domain:          "fry.local",
		MaxConnections:  4,
		MaxInFlight:     2,
		MaxRecipients:   8,
		MaxMessageBytes: maxMessage,
		IdleTimeout:     2 * time.Second,
		IngestTimeout:   5 * time.Second,
		Routes: []Route{
			{Address: "billing@example.test", Organization: f.org, Inboxes: []receiptstore.ID{id(t)}},
		},
	}
	return f, memberIDs
}

func TestSMTPPrereadCapacityRejectionReturns451AndPreservesSession(t *testing.T) {
	f, memberIDs := setupPrereadCapacity(t)
	f.start(t)
	client := f.client(t)
	const original = "Subject: original\r\n\r\naccepted before capacity exhaustion\r\n"
	{
		err := send(t, client, []string{"billing@example.test"}, original)
		requireTestCondition(t, err == nil, "initial real bounded receipt failed", err)
	}
	requireTestCondition(t, f.count(t, "fry_receipt") == 1, "initial receipt missing")
	// One accepted original exhausts the actual frozen record ceiling on both
	// members. This rejection happens in Store.Accept before source.Read.
	rejected := "Subject: rejected at prepare\r\n\r\n" + strings.Repeat("capacity-deferred-original\r\n", 3500)
	err := send(t, client, []string{"billing@example.test"}, rejected)
	requireTestCondition(t, f.count(t, "fry_receipt") == 1, "capacity-rejected original was accepted")
	requirePrereadRejectedOriginal(t, f, client, rejected, err)
	for _, memberID := range memberIDs {
		{
			err = f.store.RotateHot(context.Background(), memberID)
			requireTestCondition(t, err == nil, "explicit rotation did not recover capacity", err)
		}
	}
	const recovered = "Subject: recovered\r\n\r\naccepted on the same SMTP connection\r\n"
	{
		err = send(t, client, []string{"billing@example.test"}, recovered)
		requireTestCondition(t, err == nil, "same session could not accept after capacity recovery", err)
	}
	{
		err = client.Noop()
		requireTestCondition(t, err == nil, "successful recovery left SMTP session unusable", err)
	}
	requireTestCondition(
		t,
		(f.count(t, "fry_receipt") == 2 && f.original(t) == original),
		"recovery changed accepted originals or receipt count",
	)
	digest := sha256.Sum256([]byte(recovered))
	var copies int
	{
		err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM fry_receipt WHERE sha256=$1`, digest[:]).
			Scan(&copies)
		requireTestCondition(t, (err == nil && copies == 1), "recovered exact original missing", copies, err)
	}
}

func TestSMTPPrereadCapacityRejectClosesUnprovenFramingAndJoins(t *testing.T) {
	for _, mode := range []string{"oversized", "truncated", "stalled", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := setupPrereadCapacity(t)
			f.config.IngestTimeout = time.Second
			f.config.IdleTimeout = 3 * time.Second
			f.start(t)
			capacityReturned := observePrereadCapacity(f)
			first := f.client(t)
			{
				err := send(t, first, []string{"billing@example.test"}, "Subject: fill\r\n\r\noriginal\r\n")
				requireTestCondition(t, err == nil, err)
			}
			testCleanup(t)(first.Close())
			raw, wire := f.wire(t)
			testCheck(t)(raw.SetDeadline(time.Now().Add(3 * time.Second)))
			{
				err := wire.PrintfLine("DATA")
				requireTestCondition(t, err == nil, err)
			}
			{
				_, _, err := wire.ReadResponse(354)
				requireTestCondition(t, err == nil, err)
			}
			if mode == "oversized" {
				testResult(t)(
					io.WriteString(
						raw,
						"Subject: oversized\r\n\r\n"+strings.Repeat("oversized line\r\n", 10000)+".\r\n",
					),
				)
			} else {
				testResult(t)(io.WriteString(raw, "Subject: unfinished\r\n\r\npartial original without terminator\r\n"))
			}
			if mode == "truncated" {
				testCheck(t)(raw.CloseWrite())
			}
			if mode == "shutdown" {
				waitPrereadCapacity(t, capacityReturned)
				joinPrereadShutdown(t, f.server, "shutdown did not join pre-read rejection/drain")
			}
			code, _, err := wire.ReadResponse(0)
			var timed net.Error
			requireTestCondition(
				t,
				((code == 0 && err != nil) && (!errors.As(err, &timed) || !timed.Timeout())),
				"unproven framing must disconnect within its deadline, without final SMTP reply",
				code,
				err,
			)
			requireTestCondition(t, f.count(t, "fry_receipt") == 1, "unframed rejected original was accepted")
			// Shutdown returns only after receipt operations have joined, making the
			// state assertion independent of a merely closed client connection.
			joinPrereadShutdown(t, f.server, "terminal rejection left unjoined storage work")
			var pending int
			{
				e := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM fry_attempt WHERE state='preparing'`).
					Scan(&pending)
				requireTestCondition(t, (e == nil && pending == 0), "rejected framing left preparation", pending, e)
			}
		})
	}
}

func TestSMTPPrereadAcceptedReplayCannotAcknowledgeUnreadDATA(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		t.Run(fmt.Sprintf("status_resolved=%t", resolved), func(t *testing.T) {
			f, _ := setupPrereadCapacity(t)
			f.start(t)
			calls := configurePrereadReplay(f, resolved)
			const original = "Subject: accepted\r\n\r\noriginal accepted before replay\r\n"
			first := f.client(t)
			{
				err := send(t, first, []string{"billing@example.test"}, original)
				requireTestCondition(t, err == nil, err)
			}
			testCleanup(t)(first.Close())
			raw, wire := f.wire(t)
			testCheck(t)(raw.SetDeadline(time.Now().Add(3 * time.Second)))
			testCheck(t)(wire.PrintfLine("DATA"))
			{
				_, _, err := wire.ReadResponse(354)
				requireTestCondition(t, err == nil, err)
			}
			testResult(t)(io.WriteString(raw, "Subject: unread second body\r\n\r\nnot the original\r\n.\r\n"))
			code, _, err := wire.ReadResponse(0)
			var timed net.Error
			requireTestCondition(
				t,
				((code == 0 && err != nil) && (!errors.As(err, &timed) || !timed.Timeout())),
				"accepted authority without framed source EOF must disconnect, never send premature250",
				code,
				err,
			)
			requireTestCondition(
				t,
				((calls.Load() == 2 && f.count(t, "fry_receipt") == 1) && f.original(t) == original),
				"real accepted replay changed receipt authority",
				calls.Load(),
			)
		})
	}
}

func observePrereadCapacity(f *fixture) <-chan struct{} {
	capacityReturned := make(chan struct{})
	var once sync.Once
	// Observe the real terminal pre-read capacity failure; acceptance itself is
	// unchanged. This proves shutdown overlaps the active rejection path.
	f.server.accept = func(
		ctx context.Context,
		attempt receiptstore.ID,
		envelope receiptstore.Envelope,
		source io.ReadCloser,
	) (
		receiptstore.Receipt,
		error,
	) {
		r, e := f.store.Accept(ctx, attempt, envelope, source)
		if errors.Is(e, hotstore.ErrCapacity) {
			once.Do(func() { close(capacityReturned) })
		}
		return r, e
	}
	return capacityReturned
}

func joinPrereadShutdown(t *testing.T, server *Server, message string) {
	t.Helper()
	shutdown, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Shutdown(shutdown) }()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, smtp.ErrServerClosed) {
			t.Error("SMTP shutdown failed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal(message)
	}
}

func configurePrereadReplay(f *fixture, resolved bool) *atomic.Int32 {
	var mu sync.Mutex
	var attempt receiptstore.ID
	var envelope receiptstore.Envelope
	var calls atomic.Int32
	// The first transaction publishes a real original. Later calls exercise the
	// actual Store.Accept replay path, which returns that receipt without reading
	// the new DATA source. No accepted receipt or status result is fabricated.
	f.server.accept = func(
		ctx context.Context,
		current receiptstore.ID,
		e receiptstore.Envelope,
		source io.ReadCloser,
	) (
		receiptstore.Receipt,
		error,
	) {
		if calls.Add(1) == 1 {
			r, err := f.store.Accept(ctx, current, e, source)
			if err == nil {
				mu.Lock()
				attempt, envelope = current, e
				mu.Unlock()
			}
			return r, err
		}
		mu.Lock()
		original, originalEnvelope := attempt, envelope
		mu.Unlock()
		r, err := f.store.Accept(ctx, original, originalEnvelope, source)
		if err == nil && resolved {
			return r, receiptstore.ErrUncertain
		}
		return r, err
	}
	f.server.status = func(ctx context.Context, org, current receiptstore.ID) (receiptstore.Status, error) {
		mu.Lock()
		original := attempt
		mu.Unlock()
		return f.store.Status(ctx, org, original)
	}
	return &calls
}

func waitPrereadCapacity(t *testing.T, capacityReturned <-chan struct{}) {
	t.Helper()
	select {
	case <-capacityReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("real pre-read capacity rejection did not start")
	}
}

func openPrereadMembers(
	t *testing.T,
	f *fixture,
	ctx context.Context,
	pool *pgxpool.Pool,
	maxMessage int64,
) [2]receiptstore.ID {
	t.Helper()
	var memberIDs [2]receiptstore.ID
	encoded, err := hotstore.EncodedRecordBytes(maxMessage)
	requireTestCondition(t, err == nil, err)
	fileBytes := 64 + encoded
	opts := hotstore.Options{
		MaxMessageBytes:      maxMessage,
		MaxRecords:           1,
		MaxFileBytes:         fileBytes,
		MaxConcurrentAppends: 2,
		MaxBytes:             3*fileBytes + 4*encoded,
	}
	for i := range f.members {
		memberIDs[i] = id(t)
		m, e := receiptstore.OpenLocalMember(
			ctx,
			pool,
			memberIDs[i],
			fmt.Sprint("preread-domain-", i),
			t.TempDir(),
			opts,
		)
		requireTestCondition(t, e == nil, e)
		f.members[i] = m
		t.Cleanup(func() {
			if e := m.Close(); e != nil {
				t.Error(e)
			}
		})
	}
	return memberIDs
}

func requirePrereadRejectedOriginal(t *testing.T, f *fixture, client *mailsmtp.Client, rejected string, err error) {
	t.Helper()
	digest := sha256.Sum256([]byte(rejected))
	var copies int
	{
		e := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM fry_receipt WHERE sha256=$1`, digest[:]).
			Scan(&copies)
		requireTestCondition(t, (e == nil && copies == 0), "rejected exact original became durable", copies, e)
	}
	var reply *textproto.Error
	requireTestConditionf(
		t,
		(errors.As(err, &reply) && reply.Code == 451),
		"definite preread capacity rejection must return SMTP451, got %T: %v",
		err,
		err,
	)
	{
		err = client.Noop()
		requireTestCondition(t, err == nil, "DATA rejection destroyed reusable SMTP framing/session", err)
	}
	var preparing int
	{
		err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM fry_attempt WHERE state='preparing'`).
			Scan(&preparing)
		requireTestCondition(
			t,
			(err == nil && preparing == 0),
			"definite rejection left unsettled preparation",
			preparing,
			err,
		)
	}
}
