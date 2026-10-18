//go:build linux

package receiptstore

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"
)

// The listener and connection boundaries vary only kernel accept/close outcomes.
// All connection payload and close behavior still uses actual net.Pipe handles.
type queuedMemberListener struct {
	connections []net.Conn
	next        int
}

func (listener *queuedMemberListener) Accept() (net.Conn, error) {
	if listener.next == len(listener.connections) {
		return nil, net.ErrClosed
	}
	connection := listener.connections[listener.next]
	listener.next++
	return connection, nil
}
func (*queuedMemberListener) Close() error            { return nil }
func (listener *queuedMemberListener) Addr() net.Addr { return listener.connections[0].LocalAddr() }

type rejectedCloseFailure struct {
	net.Conn
	release func() error
}

func (connection *rejectedCloseFailure) Close() error {
	return errors.Join(connection.Conn.Close(), connection.release(), errors.New("private peer/path close detail"))
}

func TestMemberListenerContinuesAfterRejectedConnectionCloseFailure(t *testing.T) {
	first, firstPeer := net.Pipe()
	rejected, rejectedPeer := net.Pipe()
	next, nextPeer := net.Pipe()
	for _, connection := range []net.Conn{first, firstPeer, rejected, rejectedPeer, next, nextPeer} {
		t.Cleanup(func() {
			if err := connection.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	var diagnostics bytes.Buffer
	listener := &memberListener{slots: make(chan struct{}, 1), errorLog: log.New(&diagnostics, "", 0)}
	failedClose := &rejectedCloseFailure{Conn: rejected}
	listener.Listener = &queuedMemberListener{connections: []net.Conn{first, failedClose, next}}
	admitted, err := listener.Accept()
	requireReceiptNoError(t, err)
	// The failed rejected close also coincides with an existing slot being freed;
	// the next Accept must return the later connection without killing the server.
	failedClose.release = admitted.Close
	connection, err := listener.Accept()
	requireReceiptCondition(t, err == nil, "rejected connection close stopped listener admission", err)
	t.Cleanup(func() {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
	})
	requireReceiptNoError(t, connection.SetDeadline(time.Now().Add(2*time.Second)))
	written := make(chan error, 1)
	go func() { _, err := io.WriteString(nextPeer, "later connection"); written <- err }()
	data := make([]byte, len("later connection"))
	if _, err := io.ReadFull(connection, data); err != nil {
		t.Fatal(err)
	}
	requireReceiptNoError(t, <-written)
	requireReceiptCondition(t, string(data) == "later connection", "wrong connection admitted")
	requireReceiptCondition(
		t,
		diagnostics.String() == "receiptstore: rejected connection close failed\n" &&
			!strings.Contains(diagnostics.String(), "private peer/path"),
		"close failure diagnostic was missing or disclosed connection details",
	)
}
