package smtpserver

import (
	"context"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/outbound"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/emersion/go-smtp"
)

func submitQueued(t *testing.T, f *fixture, relay receiptstore.ID, recipient string) receiptstore.Receipt {
	t.Helper()
	r, err := f.store.Accept(
		context.Background(),
		id(t),
		receiptstore.Envelope{
			Organization: f.org,
			MailFrom:     "sender@example.net",
			Outbound:     &receiptstore.Outbound{Relay: relay, Recipients: []string{recipient}, RetryWindow: time.Hour},
		},
		io.NopCloser(strings.NewReader("From: sender@example.net\r\nSubject: outgoing\r\n\r\noriginal\r\n")),
	)
	requireTestCondition(t, err == nil, err)
	return r
}

func TestOutboundHandoffAndPermanentRejection(t *testing.T) {
	source, target := setup(t, 1<<20), setup(t, 1<<20)
	target.start(t)
	ctx := context.Background()
	relay := id(t)
	worker, err := outbound.New(
		source.store,
		"fry.local",
		[]outbound.Relay{{ID: relay, Address: target.addr, Transport: "loopback"}},
	)
	requireTestCondition(t, err == nil, err)
	receipt := submitQueued(t, source, relay, "billing@example.test")
	did, err := worker.RunOnce(ctx)
	requireTestCondition(t, (err == nil && did), did, err)
	state, err := source.store.OutboundState(ctx, source.org, receipt.ID)
	requireTestCondition(t, (err == nil && state["billing@example.test"] == "handed-off"), state, err)
	var count int
	{
		err = target.pool.QueryRow(ctx, `SELECT count(*) FROM fry_receipt`).Scan(&count)
		requireTestCondition(t, (err == nil && count == 1), count, err)
	}
	rejected := submitQueued(t, source, relay, "unknown@example.test")
	{
		_, err = worker.RunOnce(ctx)
		requireTestCondition(t, err == nil, err)
	}
	state, err = source.store.OutboundState(ctx, source.org, rejected.ID)
	requireTestCondition(t, (err == nil && state["unknown@example.test"] == "failed"), state, err)
	{
		err = source.pool.QueryRow(ctx, `SELECT count(*) FROM fry_failure_notice`).Scan(&count)
		requireTestCondition(t, (err == nil && count == 1), "failure notice intent absent", count, err)
	}
}

type uncertainRelaySession struct {
	conn  net.Conn
	calls *atomic.Int32
}

func (s *uncertainRelaySession) Reset()                               {}
func (s *uncertainRelaySession) Logout() error                        { return nil }
func (s *uncertainRelaySession) Mail(string, *smtp.MailOptions) error { return nil }
func (s *uncertainRelaySession) Rcpt(string, *smtp.RcptOptions) error { return nil }
func (s *uncertainRelaySession) Data(r io.Reader) error {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	if s.calls.Add(1) == 1 {
		return s.conn.Close()
	}
	return nil
}

func TestOutboundLostFinalReplyRetriesSameQueuedRecipient(t *testing.T) {
	f := setup(t, 1<<20)
	ctx := context.Background()
	var calls atomic.Int32
	relayServer := smtp.NewServer(smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) {
		return &uncertainRelaySession{conn: c.Conn(), calls: &calls}, nil
	}))
	relayServer.Domain = "relay.local"
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	requireTestCondition(t, err == nil, err)
	go func() {
		testCleanup(t)(relayServer.Serve(listener))
	}()
	defer func() {
		testCleanup(t)(relayServer.Close())
	}()
	relay := id(t)
	worker, err := outbound.New(
		f.store,
		"fry.local",
		[]outbound.Relay{{ID: relay, Address: listener.Addr().String(), Transport: "loopback"}},
	)
	requireTestCondition(t, err == nil, err)
	receipt := submitQueued(t, f, relay, "receiver@example.net")
	{
		_, err = worker.RunOnce(ctx)
		requireTestCondition(t, err == nil, err)
	}
	state, err := f.store.OutboundState(ctx, f.org, receipt.ID)
	requireTestCondition(
		t,
		((err == nil && state["receiver@example.net"] == "queued") && calls.Load() == 1),
		state,
		calls.Load(),
		err,
	)
	{
		_, err = f.pool.Exec(ctx, `UPDATE fry_outbound SET available_at=clock_timestamp()-interval '1 second'`)
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err = worker.RunOnce(ctx)
		requireTestCondition(t, err == nil, err)
	}
	state, err = f.store.OutboundState(ctx, f.org, receipt.ID)
	requireTestCondition(
		t,
		((err == nil && state["receiver@example.net"] == "handed-off") && calls.Load() == 2),
		state,
		calls.Load(),
		err,
	)
}
