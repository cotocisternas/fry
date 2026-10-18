//go:build linux

package receiptstore

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// registrationCommitGate models a member descheduled after PostgreSQL has made
// one read registration visible: the COMMIT acknowledgement for that operation
// is withheld from pgx. The tracer identifies the registration by its INSERT
// and captures the contexts pgx watches for both statements.
type registrationCommitGate struct {
	operation string
	holds     chan *registrationCommit
	released  chan struct{}
	stopped   chan struct{}
	once      sync.Once
}

type registrationCommit struct {
	operation, commit context.Context
	// Closed only after pgx's cancellation deadline reached the socket.
	interrupted chan struct{}
	once        sync.Once
}

func newRegistrationCommitGate(t *testing.T, operation ID) *registrationCommitGate {
	t.Helper()
	g := &registrationCommitGate{
		operation: operation.String(),
		holds:     make(chan *registrationCommit, 1),
		released:  make(chan struct{}),
		stopped:   make(chan struct{}),
	}
	t.Cleanup(func() { g.release(); close(g.stopped) })
	return g
}

func (g *registrationCommitGate) release() { g.once.Do(func() { close(g.released) }) }

func (g *registrationCommitGate) configure(c *pgxpool.Config) {
	dial := c.ConnConfig.DialFunc
	c.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &registrationCommitConn{Conn: conn, gate: g}, nil
	}
	c.ConnConfig.Tracer = g
}

func (g *registrationCommitGate) TraceQueryStart(
	ctx context.Context,
	conn *pgx.Conn,
	data pgx.TraceQueryStartData,
) context.Context {
	wire, ok := conn.PgConn().Conn().(*registrationCommitConn)
	if !ok {
		return ctx
	}
	if data.SQL == dbgen.MemberOwnerReadInsertMemberRead && len(data.Args) > 0 && data.Args[0] == g.operation {
		wire.register(ctx)
	}
	if data.SQL == "commit" {
		wire.commit(ctx)
	}
	return ctx
}

func (g *registrationCommitGate) TraceQueryEnd(_ context.Context, conn *pgx.Conn, _ pgx.TraceQueryEndData) {
	if wire, ok := conn.PgConn().Conn().(*registrationCommitConn); ok {
		wire.settled()
	}
}

// hold withholds the acknowledgement until the test releases it, and reports
// whether pgx interrupted it instead: a COMMIT whose own context was cancelled
// waits for pgx to apply its socket deadline.
func (g *registrationCommitGate) hold(held *registrationCommit) bool {
	g.holds <- held
	select {
	case <-held.commit.Done():
	case <-g.released:
	}
	if held.commit.Err() == nil {
		return false
	}
	select {
	case <-held.interrupted:
	case <-g.stopped:
	}
	return true
}

func (g *registrationCommitGate) await(t *testing.T) *registrationCommit {
	t.Helper()
	select {
	case held := <-g.holds:
		return held
	case <-time.After(5 * time.Second):
		t.Fatal("read registration COMMIT acknowledgement was not withheld")
		return nil
	}
}

type registrationCommitConn struct {
	net.Conn
	gate              *registrationCommitGate
	mu                sync.Mutex
	operation         context.Context
	pending, inflight *registrationCommit
}

func (c *registrationCommitConn) register(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.operation = ctx
}

func (c *registrationCommitConn) commit(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.operation == nil {
		return
	}
	c.pending = &registrationCommit{operation: c.operation, commit: ctx, interrupted: make(chan struct{})}
	c.inflight, c.operation = c.pending, nil
}

func (c *registrationCommitConn) settled() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending, c.inflight = nil, nil
}

// Read gates delivered bytes, not calls: pgx's background reader may already
// be blocked here when COMMIT is sent. After COMMIT no earlier reply remains,
// so the first bytes are its acknowledgement. If pgx interrupts the hold, they
// count as unread and the read fails as the deadlined socket would.
func (c *registrationCommitConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n == 0 {
		return n, err
	}
	c.mu.Lock()
	held := c.pending
	c.pending = nil
	c.mu.Unlock()
	if held == nil || !c.gate.hold(held) {
		return n, err
	}
	return c.Conn.Read(p)
}

// pgx's default context watcher interrupts a cancelled statement with a past
// deadline. Apply it to the socket first, so the woken read cannot receive the
// acknowledgement before the interruption is effective.
func (c *registrationCommitConn) SetDeadline(deadline time.Time) error {
	err := c.Conn.SetDeadline(deadline)
	c.mu.Lock()
	held := c.inflight
	c.mu.Unlock()
	if held != nil && !deadline.IsZero() {
		held.once.Do(func() { close(held.interrupted) })
	}
	return err
}

// Ownership can end after PostgreSQL commits a read registration but before
// the member reads that acknowledgement. No storage call has begun, so the
// durable registration must still receive closure proof rather than debt.
func TestMemberGRPCOwnerCancellationDuringReadRegistrationCommitProvesClosure(t *testing.T) {
	operation := newID(t)
	gate := newRegistrationCommitGate(t, operation)
	f := setupWithPoolConfig(
		t,
		gate.configure,
		hotOptions(),
		Options{Lease: 10 * time.Second, RecoveryRetention: 24 * time.Hour},
	)
	members, servers := remoteFixture(t, f)
	stream, req := unreceivedMemberRead(t, f, members, operation)
	requireReceiptNoError(t, stream.SendMsg(req))
	requireReceiptNoError(t, stream.CloseSend())
	held := gate.await(t)
	// Visible to this poll while the acknowledgement is withheld from pgx.
	waitMemberReadState(t, f, operation, false)
	servers[0].owner.cancel()
	deadline := time.Now().Add(5 * time.Second)
	select {
	case <-held.operation.Done():
	case <-time.After(time.Until(deadline)):
		t.Fatal("owner cancellation did not reach the read registration")
	}
	// Done closes before derived contexts are cancelled. Cause takes the lock
	// that cancellation holds until all of them are, so a COMMIT still bound to
	// the operation is interrupted by pgx; a detached COMMIT proceeds.
	requireReceiptCondition(t, context.Cause(held.operation) != nil, "read registration was not cancelled")
	gate.release()
	requireClosedMemberRead(t, f, operation, deadline)
}
