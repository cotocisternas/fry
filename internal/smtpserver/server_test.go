package smtpserver

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	mailsmtp "net/smtp"
	"net/textproto"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/emersion/go-smtp"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	pool       *pgxpool.Pool
	store      *receiptstore.Store
	members    [2]*receiptstore.LocalMember
	org, other receiptstore.ID
	config     Config
	server     *Server
	addr       string
}

func id(t *testing.T) receiptstore.ID {
	t.Helper()
	v, err := hotstore.NewID()
	requireTestCondition(t, err == nil, err)
	return v
}

func setup(t *testing.T, limit int64) *fixture {
	t.Helper()
	dsn := os.Getenv("FRY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("run scripts/tests/receipts.sh for PostgreSQL-backed SMTP tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	requireTestCondition(t, err == nil, err)
	t.Cleanup(admin.Close)
	schema := "fry_smtp_" + id(t).String()
	quoted := pgx.Identifier{schema}.Sanitize()
	{
		_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
		requireTestCondition(t, err == nil, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
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
	openSMTPMembers(t, f, ctx, pool)
	f.store, err = receiptstore.New(
		ctx,
		pool,
		f.members,
		receiptstore.Options{Lease: time.Second, RecoveryRetention: time.Hour},
	)
	requireTestCondition(t, err == nil, err)
	inboxA, inboxB := id(t), id(t)
	f.config = Config{
		Domain:          "fry.local",
		MaxConnections:  8,
		MaxInFlight:     4,
		MaxRecipients:   100,
		MaxMessageBytes: limit,
		IdleTimeout:     2 * time.Second,
		IngestTimeout:   5 * time.Second,
		Routes: []Route{
			{Address: "invoices@example.test", Organization: f.org, Inboxes: []receiptstore.ID{inboxA, inboxB}},
			{Address: "billing@example.test", Organization: f.org, Inboxes: []receiptstore.ID{inboxA}},
			{Address: "other@second.test", Organization: f.other, Inboxes: []receiptstore.ID{id(t)}},
		},
	}
	return f
}

func (f *fixture) start(t *testing.T) {
	t.Helper()
	f.startObservingSessions(t, nil)
}

// startObservingSessions exposes the public backend contract before Serve starts.
func (f *fixture) startObservingSessions(t *testing.T, sessions chan<- *observedBackendSession) {
	t.Helper()
	s, err := New(f.store, f.config)
	requireTestCondition(t, err == nil, err)
	if sessions != nil {
		backend := s.smtp.Backend
		s.smtp.Backend = smtp.BackendFunc(func(conn *smtp.Conn) (smtp.Session, error) {
			session, err := backend.NewSession(conn)
			if err != nil {
				return nil, err
			}
			observed := &observedBackendSession{Session: session, loggedOut: make(chan struct{})}
			sessions <- observed
			return observed, nil
		})
	}
	f.server = s
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	requireTestCondition(t, err == nil, err)
	f.addr = listener.Addr().String()
	done := make(chan error, 1)
	go func() { done <- s.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		testCleanup(t)(s.Shutdown(ctx))
		testCleanup(t)(listener.Close())
		select {
		case err := <-done:
			testCleanup(t)(err)
		case <-time.After(3 * time.Second):
			t.Error("SMTP serving did not stop")
		}
	})
}

func (f *fixture) client(t *testing.T) *mailsmtp.Client {
	t.Helper()
	raw, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", f.addr)
	requireTestCondition(t, err == nil, err)
	testCheck(t)(raw.SetDeadline(time.Now().Add(5 * time.Second)))
	c, err := mailsmtp.NewClient(raw, "fry.local")
	requireTestCondition(t, err == nil, err)
	t.Cleanup(func() { testCleanup(t)(c.Close()) })
	return c
}

func send(t *testing.T, c *mailsmtp.Client, recipients []string, body string) error {
	t.Helper()
	if err := c.Mail("sender@partner.test"); err != nil {
		return err
	}
	for _, r := range recipients {
		if err := c.Rcpt(r); err != nil {
			return err
		}
	}
	writer, err := c.Data()
	if err != nil {
		return err
	}
	if _, err = io.WriteString(writer, body); err != nil {
		return err
	}
	return writer.Close()
}

func (f *fixture) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	{
		err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&n)
		requireTestCondition(t, err == nil, err)
	}
	return n
}

func (f *fixture) original(t *testing.T) string {
	t.Helper()
	var raw string
	var size int64
	{
		err := f.pool.QueryRow(
			context.Background(),
			"SELECT id,raw_size FROM fry_receipt ORDER BY received_at LIMIT 1",
		).
			Scan(&raw, &size)
		requireTestCondition(t, err == nil, err)
	}
	b, err := hex.DecodeString(raw)
	requireTestCondition(t, err == nil, err)
	var id receiptstore.ID
	copy(id[:], b)
	var out bytes.Buffer
	{
		err = f.store.Read(context.Background(), f.org, id, 0, size, &out)
		requireTestCondition(t, err == nil, err)
	}
	return out.String()
}

func TestSMTPDurableAliasDeliveryAndConnectionReuse(t *testing.T) {
	f := setup(t, 1<<20)
	f.start(t)
	client := f.client(t)
	body := "From: sender@partner.test\r\nSubject: XML exchange\r\n\r\n.unchanged\r\n<invoice/>\r\n"
	{
		err := send(t, client, []string{"invoices@example.test", "billing@example.test"}, body)
		requireTestCondition(t, err == nil, err)
	}
	requireTestCondition(
		t,
		((f.count(t, "fry_receipt") == 1 && f.count(t, "fry_destination") == 2) && f.count(t, "fry_target") == 2),
		"incorrect durable fan-out",
	)
	{
		got := f.original(t)
		requireTestConditionf(t, got == body, "raw MIME changed: %q", got)
	}
	{
		err := send(t, client, []string{"billing@example.test"}, body)
		requireTestCondition(t, err == nil, "success closed reusable connection", err)
	}
	requireTestCondition(t, f.count(t, "fry_receipt") == 2, "separate SMTP transaction deduplicated")
}

func TestSMTPRejectsRelayAndDefersCrossOrganizationRecipients(t *testing.T) {
	f := setup(t, 1024)
	f.start(t)
	c := f.client(t)
	{
		err := c.Mail("")
		requireTestCondition(t, err == nil, err)
	}
	for _, address := range []string{"outside@unconfigured.test", "missing@example.test"} {
		err := c.Rcpt(address)
		var smtpErr *textproto.Error
		requireTestCondition(t, (errors.As(err, &smtpErr) && smtpErr.Code == 550), "unknown recipient accepted", err)
	}
	{
		err := c.Rcpt("billing@example.test")
		requireTestCondition(t, err == nil, err)
	}
	err := c.Rcpt("other@second.test")
	var smtpErr *textproto.Error
	requireTestCondition(t, (errors.As(err, &smtpErr) && smtpErr.Code == 451), "organizations mixed", err)
	writer, err := c.Data()
	requireTestCondition(t, err == nil, err)
	testResult(t)(io.WriteString(writer, "message\r\n"))
	{
		err = writer.Close()
		requireTestCondition(t, err == nil, err)
	}
	requireTestCondition(t, f.count(t, "fry_receipt") == 1, "wrong receipt count")
}

func TestSMTPSizeBoundaries(t *testing.T) {
	for _, body := range []string{"ab\r\n", "abc\r\n", "abcd\r\n"} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			f := setup(t, 5)
			f.start(t)
			err := send(t, f.client(t), []string{"billing@example.test"}, body)
			if len(body) <= 5 {
				requireTestCondition(t, err == nil && f.original(t) == body, "legal boundary rejected", err)
			} else {
				requireTestCondition(
					t,
					err != nil && f.count(t, "fry_receipt") == 0,
					"oversized original accepted",
					err,
				)
			}
		})
	}
}

func TestSMTPWaitsForDurableAuthority(t *testing.T) {
	f := setup(t, 1024)
	f.start(t)
	c := f.client(t)
	tx, err := f.pool.Begin(context.Background())
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(tx.Rollback(context.Background()))
	}()
	{
		_, err = tx.Exec(context.Background(), "SELECT * FROM fry_profile FOR UPDATE")
		requireTestCondition(t, err == nil, err)
	}
	done := make(chan error, 1)
	go func() { done <- send(t, c, []string{"billing@example.test"}, "wait for commit\r\n") }()
	select {
	case err := <-done:
		t.Fatal("responded before authority became available", err)
	case <-time.After(50 * time.Millisecond):
	}
	requireTestCondition(t, f.count(t, "fry_receipt") == 0, "receipt bypassed profile guard")
	{
		err = tx.Commit(context.Background())
		requireTestCondition(t, err == nil, err)
	}
	{
		err = <-done
		requireTestCondition(t, err == nil, err)
	}
	requireTestCondition(t, f.count(t, "fry_receipt") == 1, "success lacks durable receipt")
}

func TestSMTPMemberFailureCannotAcknowledge(t *testing.T) {
	f := setup(t, 1024)
	f.start(t)
	testCleanup(t)(f.members[1].Close())
	{
		err := send(t, f.client(t), []string{"billing@example.test"}, "cannot store\r\n")
		requireTestCondition(t, err != nil, "accepted with failed member")
	}
	requireTestCondition(t, f.count(t, "fry_receipt") == 0, "failed receipt became visible")
}

func (f *fixture) wire(t *testing.T) (*net.TCPConn, *textproto.Conn) {
	t.Helper()
	addr, err := net.ResolveTCPAddr("tcp", f.addr)
	requireTestCondition(t, err == nil, err)
	raw, err := net.DialTCP("tcp", nil, addr)
	requireTestCondition(t, err == nil, err)
	testCheck(t)(raw.SetDeadline(time.Now().Add(5 * time.Second)))
	t.Cleanup(func() { testCleanup(t)(raw.Close()) })
	conn := textproto.NewConn(raw)
	{
		_, _, err = conn.ReadResponse(220)
		requireTestCondition(t, err == nil, err)
	}
	for _, cmd := range []string{
		"EHLO partner.test",
		"MAIL FROM:<sender@partner.test>",
		"RCPT TO:<billing@example.test>",
	} {
		{
			err = conn.PrintfLine("%s", cmd)
			requireTestCondition(t, err == nil, err)
		}
		{
			_, _, err = conn.ReadResponse(250)
			requireTestCondition(t, err == nil, err)
		}
	}
	return raw, conn
}

func TestSMTPRejectsTruncatedDATAAndBDAT(t *testing.T) {
	for _, mode := range []string{"data", "first-chunk", "last-chunk", "empty-last", "later-last"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t, 1024)
			f.start(t)
			raw, c := f.wire(t)
			switch mode {
			case "data":
				testCheck(t)(c.PrintfLine("DATA"))
				{
					_, _, err := c.ReadResponse(354)
					requireTestCondition(t, err == nil, err)
				}
				testResult(t)(io.WriteString(raw, "unterminated body\r\n"))
			case "first-chunk":
				testResult(t)(io.WriteString(raw, "BDAT 5\r\nabc"))
			case "last-chunk":
				testResult(t)(io.WriteString(raw, "BDAT 5 LAST\r\nabc"))
			case "empty-last":
				testResult(t)(io.WriteString(raw, "BDAT 5 LAST\r\n"))
			case "later-last":
				testResult(t)(io.WriteString(raw, "BDAT 3\r\nabc"))
				{
					_, _, err := c.ReadResponse(250)
					requireTestCondition(t, err == nil, err)
				}
				testResult(t)(io.WriteString(raw, "BDAT 5 LAST\r\ndef"))
			}
			testCheck(t)(raw.CloseWrite())
			code, _, readErr := c.ReadResponse(0)
			var responseErr *textproto.Error
			requireTestConditionf(
				t,
				(((readErr == nil || errors.As(readErr, &responseErr)) ||
					errors.Is(readErr, io.EOF)) ||
					errors.Is(readErr, net.ErrClosed)),
				"expected truncated-frame rejection or disconnect: %v",
				readErr,
			)
			requireTestCondition(t, code != 250, "truncated frame acknowledged")
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			testCleanup(t)(f.server.Shutdown(ctx))
			cancel()
			requireTestCondition(t, f.count(t, "fry_receipt") == 0, "truncated original committed")
		})
	}
}

func TestSMTPZeroLengthBDATIsACompleteMessage(t *testing.T) {
	f := setup(t, 1024)
	f.start(t)
	raw, c := f.wire(t)
	testResult(t)(io.WriteString(raw, "BDAT 0 LAST\r\n"))
	{
		_, _, err := c.ReadResponse(250)
		requireTestCondition(t, err == nil, err)
	}
	requireTestCondition(t, (f.count(t, "fry_receipt") == 1 && f.original(t) == ""), "empty original lost")
}

func TestSMTPCommitAmbiguityIsResolvedOrConnectionClosed(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprint(unavailable), func(t *testing.T) {
			f := setup(t, 1024)
			f.start(t)
			f.server.accept = func(
				ctx context.Context,
				id receiptstore.ID,
				e receiptstore.Envelope,
				r io.ReadCloser,
			) (
				receiptstore.Receipt,
				error,
			) {
				receipt, err := f.store.Accept(ctx, id, e, r)
				if err != nil {
					return receipt, err
				}
				return receipt, receiptstore.ErrUncertain
			}
			if unavailable {
				f.server.status = func(context.Context, receiptstore.ID, receiptstore.ID) (receiptstore.Status, error) {
					return receiptstore.Status{}, io.ErrUnexpectedEOF
				}
			}
			err := send(t, f.client(t), []string{"billing@example.test"}, "committed\r\n")
			requireTestCondition(t, (unavailable || err == nil), "could not resolve committed receipt", err)
			if unavailable {
				var response *textproto.Error
				requireTestCondition(
					t,
					(err != nil && !errors.As(err, &response)),
					"uncertain commit reported as definite outcome",
					err,
				)
			}
			requireTestCondition(t, f.count(t, "fry_receipt") == 1, "committed receipt lost")
		})
	}
}

func TestSMTPAdmissionResetAndForcedShutdown(t *testing.T) {
	f := setup(t, 1024)
	f.config.MaxInFlight = 1
	f.start(t)
	first := f.client(t)
	second := f.client(t)
	{
		err := first.Mail("a@partner.test")
		requireTestCondition(t, err == nil, err)
	}
	err := second.Mail("b@partner.test")
	var response *textproto.Error
	requireTestCondition(t, (errors.As(err, &response) && response.Code == 451), "unbounded ingestion admission", err)
	{
		err = first.Reset()
		requireTestCondition(t, err == nil, err)
	}
	{
		err = second.Mail("b@partner.test")
		requireTestCondition(t, err == nil, "reset leaked slot", err)
	}
	{
		err = second.Rcpt("billing@example.test")
		requireTestCondition(t, err == nil, err)
	}
	writer, err := second.Data()
	requireTestCondition(t, err == nil, err)
	testResult(t)(io.WriteString(writer, strings.Repeat("pending", 20)))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	{
		err = f.server.Shutdown(ctx)
		requireTestCondition(t, err != nil, "test did not force shutdown")
	}
	requireTestCondition(t, f.count(t, "fry_receipt") == 0, "shutdown accepted incomplete body")
}

func openSMTPMembers(t *testing.T, f *fixture, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for i := range f.members {
		m, err := receiptstore.OpenLocalMember(
			ctx,
			pool,
			id(t),
			fmt.Sprint("domain-", i),
			t.TempDir(),
			hotstore.Options{MaxMessageBytes: 1 << 20, MaxBytes: 16 << 20, MaxConcurrentAppends: 4},
		)
		requireTestCondition(t, err == nil, err)
		f.members[i] = m
		t.Cleanup(func() { testCleanup(t)(m.Close()) })
	}
}
