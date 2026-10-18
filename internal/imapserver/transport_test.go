//go:build linux

package imapserver

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
)

func transportTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	requireTestCondition(t, err == nil, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	requireTestCondition(t, err == nil, err)
	cert, err := x509.ParseCertificate(der)
	requireTestCondition(t, err == nil, err)
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	}, &tls.Config{
		RootCAs:    roots,
		ServerName: "127.0.0.1",
		MinVersion: tls.VersionTLS12,
	}
}

func transportServer(t *testing.T, f *fixture, opts Options) (*Server, string, <-chan error) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	requireTestCondition(t, err == nil, err)
	return transportServerOnListener(t, f, opts, listener)
}

func transportServerOnListener(
	t *testing.T,
	f *fixture,
	opts Options,
	listener net.Listener,
) (*Server, string, <-chan error) {
	t.Helper()
	server, err := NewWithOptions(
		f.store,
		[]Account{{Username: "test", Password: "synthetic-only-password", Organization: f.org, Inbox: f.inbox}},
		opts,
	)
	requireTestCondition(t, err == nil, err)
	t.Cleanup(func() { testCleanup(t)(server.Close()) })
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	return server, listener.Addr().String(), done
}

func TestImplicitTLSLoginAndCertificateVerification(t *testing.T) {
	f := setup(t)
	serverTLS, clientTLS := transportTLS(t)
	_, address, _ := transportServer(t, f, Options{Transport: "tls", TLS: serverTLS})
	client, err := imapclient.DialTLS(address, &imapclient.Options{TLSConfig: clientTLS})
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(client.Close())
	}()
	{
		err = client.Login("test", "synthetic-only-password").Wait()
		requireTestCondition(t, err == nil, "TLS state not recognized", err)
	}
	{
		_, err = client.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	wrong := clientTLS.Clone()
	wrong.ServerName = "wrong.example.test"
	if connection, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: time.Second}, Config: wrong}).DialContext(
		t.Context(),
		"tcp",
		address,
	); err == nil {
		testCleanup(t)(connection.Close())
		t.Fatal("wrong hostname accepted")
	}
	old := clientTLS.Clone()
	old.MinVersion = tls.VersionTLS10
	old.MaxVersion = tls.VersionTLS11
	if connection, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: time.Second}, Config: old}).DialContext(
		t.Context(),
		"tcp",
		address,
	); err == nil {
		testCleanup(t)(connection.Close())
		t.Fatal("obsolete TLS accepted")
	}
}

func TestImplicitTLSBoundsHandshakeAndTotalConnections(t *testing.T) {
	f := setup(t)
	serverTLS, clientTLS := transportTLS(t)
	raw, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	requireTestCondition(t, err == nil, err)
	paused := &pausedTransportCloseListener{Listener: raw, reading: make(chan struct{}), release: make(chan struct{})}
	server, address, done := transportServerOnListener(
		t,
		f,
		Options{
			Transport:            "tls",
			TLS:                  serverTLS,
			MaxConnections:       2,
			MaxPendingHandshakes: 1,
			HandshakeTimeout:     100 * time.Millisecond,
		},
		paused,
	)
	releaseClose := sync.OnceFunc(func() { close(paused.release) })
	defer releaseClose()
	stalled, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(stalled.Close())
	}()
	// The handshake's first read proves it owns a pending-handshake slot.
	select {
	case <-paused.reading:
	case <-time.After(time.Second):
		t.Fatal("stalled handshake did not start")
	}
	rejected, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(rejected.Close())
	}()
	requireSaturatedTLSHandshakesClose(t, rejected, stalled)
	// Peer EOF precedes server-side Close completion. Keep that gap open to
	// prove the old immediate recovery dial would still be rejected here.
	waitTransportConnections(t, server, 1)
	requireTransportTLSRejected(t, address, clientTLS)
	releaseClose()
	// Closure and handshake-slot release finish asynchronously. Successful TLS
	// plus an IMAP greeting proves readiness through the actual Serve path.
	first := authenticatedTransport(t, address, clientTLS)
	_ = authenticatedTransport(t, address, clientTLS)
	waitTransportConnections(t, server, 2)

	requireTransportTLSRejected(t, address, clientTLS)
	requireTransportShutdown(t, server, done, first)
}

func requireTransportShutdown(t *testing.T, server *Server, done <-chan error, first net.Conn) {
	t.Helper()
	err := server.Close()
	requireTestCondition(t, err == nil, err)
	select {
	case err = <-done:
		requireTestCondition(t, err == nil, err)
	case <-time.After(time.Second):
		t.Fatal("TLS Serve did not stop")
	}
	testCheck(t)(first.SetReadDeadline(time.Now().Add(time.Second)))
	var one [1]byte
	{
		_, err = first.Read(one[:])
		requireTestCondition(t, err != nil, "authenticated transport survived shutdown")
	}
}

// Only the first raw connection pauses after peer EOF, before either the
// tracked connection or pending-handshake slot can be released.
type pausedTransportCloseListener struct {
	net.Listener
	accepted bool
	reading  chan struct{}
	release  chan struct{}
}

func (l *pausedTransportCloseListener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil || l.accepted {
		return connection, err
	}
	l.accepted = true
	return &pausedTransportCloseConn{Conn: connection, reading: l.reading, release: l.release}, nil
}

type pausedTransportCloseConn struct {
	net.Conn
	once     sync.Once
	readOnce sync.Once
	err      error
	reading  chan struct{}
	release  <-chan struct{}
}

func (c *pausedTransportCloseConn) Read(p []byte) (int, error) {
	c.readOnce.Do(func() { close(c.reading) })
	return c.Conn.Read(p)
}

func (c *pausedTransportCloseConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		<-c.release
	})
	return c.err
}

func TestImplicitTLSShutdownCancelsPendingHandshake(t *testing.T) {
	f := setup(t)
	serverTLS, _ := transportTLS(t)
	server, address, done := transportServer(
		t,
		f,
		Options{Transport: "tls", TLS: serverTLS, HandshakeTimeout: time.Minute},
	)
	connection, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(connection.Close())
	}()
	until := time.Now().Add(time.Second)
	for {
		server.mu.Lock()
		n := len(server.connections)
		server.mu.Unlock()
		if n != 0 {
			break
		}
		requireTestCondition(t, !time.Now().After(until), "connection not admitted")
		time.Sleep(time.Millisecond)
	}
	closed := make(chan error, 1)
	go func() { closed <- server.Close() }()
	select {
	case err = <-closed:
		requireTestCondition(t, err == nil, err)
	case <-time.After(time.Second):
		t.Fatal("shutdown waited for handshake timeout")
	}
	select {
	case err := <-done:
		testCleanup(t)(err)
	case <-time.After(time.Second):
		t.Fatal("listener not closed")
	}
}

func TestIMAPTransportRejectsPublicPlaintextAndInvalidTLS(t *testing.T) {
	f := setup(t)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "0.0.0.0:0")
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(listener.Close())
	}()
	{
		err = f.server.Serve(listener)
		requireTestCondition(
			t,
			(err != nil && strings.Contains(err.Error(), "loopback")),
			"public plaintext permitted",
			err,
		)
	}
	{
		_, err = NewWithOptions(
			f.store,
			[]Account{{Username: "test", Password: "password", Organization: f.org, Inbox: f.inbox}},
			Options{Transport: "tls"},
		)
		requireTestCondition(t, err != nil, "TLS without certificate permitted")
	}
}

func waitTransportConnections(t *testing.T, server *Server, want int) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for {
		server.mu.Lock()
		n := len(server.connections)
		server.mu.Unlock()
		if n == want {
			return
		}
		requireTestCondition(
			t,
			!time.Now().After(until),
			"transport connections did not settle", n, want,
		)
		time.Sleep(time.Millisecond)
	}
}

func requireTransportTLSRejected(t *testing.T, address string, config *tls.Config) {
	t.Helper()
	if connection, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: time.Second}, Config: config}).DialContext(
		t.Context(),
		"tcp",
		address,
	); err == nil {
		testCleanup(t)(connection.Close())
		t.Fatal("saturated TLS transport admitted a connection")
	}
}

func authenticatedTransport(t *testing.T, address string, config *tls.Config) net.Conn {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	for {
		connection, err := (&tls.Dialer{Config: config}).DialContext(ctx, "tcp", address)
		if err == nil {
			err = connection.SetReadDeadline(deadline)
			var greeting string
			if err == nil {
				greeting, err = bufio.NewReader(connection).ReadString('\n')
			}
			if err == nil {
				t.Cleanup(func() { testCleanup(t)(connection.Close()) })
				requireTestCondition(t, strings.HasPrefix(greeting, "* OK "), "unexpected IMAP greeting", greeting)
				return connection
			}
			testCleanup(t)(connection.Close())
		}
		select {
		case <-ctx.Done():
			t.Fatal("capacity did not recover", err)
		case <-time.After(time.Millisecond):
		}
	}
}

func requireSaturatedTLSHandshakesClose(t *testing.T, rejected, stalled net.Conn) {
	t.Helper()
	testCheck(t)(rejected.SetReadDeadline(time.Now().Add(time.Second)))
	var one [1]byte
	{
		n, err := rejected.Read(one[:])
		requireTestCondition(
			t,
			err != nil && n == 0 && !errors.Is(err, os.ErrDeadlineExceeded),
			"implicit overload emitted plaintext or stayed open",
			n,
			err,
		)
	}
	testCheck(t)(stalled.SetReadDeadline(time.Now().Add(time.Second)))
	{
		n, err := stalled.Read(one[:])
		requireTestCondition(
			t,
			err != nil && n == 0 && !errors.Is(err, os.ErrDeadlineExceeded),
			"stalled handshake not closed",
			n,
			err,
		)
	}
}
