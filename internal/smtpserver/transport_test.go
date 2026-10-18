package smtpserver

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"sync"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/receiptstore"
)

func transportConfig(t *testing.T, mode string) Config {
	t.Helper()
	cert, _ := relayCertificate(t)
	cfg := Config{
		Domain:           "mail.example.test",
		Transport:        mode,
		TLSConfig:        &tls.Config{Certificates: []tls.Certificate{cert}},
		HandshakeTimeout: time.Second,
		MaxConnections:   2,
		MaxInFlight:      1,
		MaxRecipients:    10,
		MaxMessageBytes:  1 << 20,
		IdleTimeout:      10 * time.Second,
		IngestTimeout:    time.Second,
	}
	if mode == "starttls" {
		cfg.Routes = []Route{{Address: "in@example.test", Organization: id(t), Inboxes: []receiptstore.ID{id(t)}}}
	} else {
		cfg.Submission = &SubmissionConfig{
			SenderAuthority: protocolSenderAuthority{},
			DefaultRelay:    id(t),
			RetryWindow:     time.Hour,
			Accounts: []SubmissionAccount{
				{
					Username:          "user",
					Password:          "synthetic-password",
					Organization:      id(t),
					AuthorizedDomains: []string{"example.test"},
				},
			},
		}
		cfg.Submission.Credentials = protocolCredentials{org: cfg.Submission.Accounts[0].Organization}
	}
	return cfg
}

func serveTransport(t *testing.T, cfg Config) (*Server, string) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	requireTestCondition(t, err == nil, err)
	return serveTransportOn(t, cfg, listener)
}

func serveTransportOn(t *testing.T, cfg Config, listener net.Listener) (*Server, string) {
	t.Helper()
	server, err := New(&receiptstore.Store{}, cfg)
	if err != nil {
		testCleanup(t)(listener.Close())
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		testCleanup(t)(server.Shutdown(ctx))
		testCleanup(t)(listener.Close())
		select {
		case err := <-done:
			testCleanup(t)(err)
		case <-time.After(time.Second):
			t.Error("listener did not stop")
		}
	})
	return server, listener.Addr().String()
}

func TestSubmissionTLSModes(t *testing.T) {
	for _, mode := range []string{"starttls-required", "tls"} {
		t.Run(mode, func(t *testing.T) {
			cfg := transportConfig(t, mode)
			cert, roots := relayCertificate(t)
			cfg.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
			server, address := serveTransport(t, cfg)
			client := submissionTLSClient(t, address, mode, roots)
			var err error
			defer func() {
				testCleanup(t)(client.Close())
			}()
			{
				supported, _ := client.Extension("AUTH")
				requireTestCondition(t, supported, "AUTH missing after TLS")
			}
			{
				err = client.Mail("user@example.test")
				requireTestCondition(t, err != nil, "unauthenticated encrypted submission accepted")
			}
			authHost := "localhost"
			if mode == "starttls-required" {
				authHost = "127.0.0.1"
			}
			{
				err = client.Auth(smtp.PlainAuth("", "user", "synthetic-password", authHost))
				requireTestCondition(t, err == nil, err)
			}
			{
				err = client.Mail("other@elsewhere.test")
				requireTestCondition(t, err != nil, "unauthorized sender accepted")
			}
			{
				err = client.Mail("user@example.test")
				requireTestCondition(t, err == nil, err)
			}
			{
				err = client.Reset()
				requireTestCondition(t, err == nil, err)
			}
			requireTestCondition(
				t,
				(server.config.TLSConfig.MinVersion == tls.VersionTLS12 && cfg.TLSConfig.MinVersion == 0),
				"TLS minimum not enforced on independent clone",
			)
		})
	}
}

func TestIncomingOptionalTLS(t *testing.T) {
	cfg := transportConfig(t, "starttls")
	_, address := serveTransport(t, cfg)
	client, err := smtp.Dial(address)
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(client.Close())
	}()
	{
		supported, _ := client.Extension("AUTH")
		requireTestCondition(t, !supported, "MX advertised AUTH")
	}
	{
		supported, _ := client.Extension("STARTTLS")
		requireTestCondition(t, supported, "MX missing STARTTLS")
	}
	{
		err = client.Mail("sender@external.test")
		requireTestCondition(t, err == nil, "MX required TLS", err)
	}
	{
		err = client.Rcpt("in@example.test")
		requireTestCondition(t, err == nil, err)
	}
	testCheck(t)(client.Reset())
}

func TestPlaintextAuthAndFailedUpgradeClose(t *testing.T) {
	_, address := serveTransport(t, transportConfig(t, "starttls-required"))
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(conn.Close())
	}()
	testCheck(t)(conn.SetDeadline(time.Now().Add(3 * time.Second)))
	reader := textproto.NewReader(bufio.NewReader(conn))
	{
		_, _, err = reader.ReadResponse(220)
		requireTestCondition(t, err == nil, err)
	}
	testResult(t)(fmt.Fprint(conn, "EHLO fixture\r\n"))
	{
		_, _, err = reader.ReadResponse(250)
		requireTestCondition(t, err == nil, err)
	}
	testResult(t)(fmt.Fprint(conn, "AUTH PLAIN not-even-base64\r\n"))
	{
		_, _, err = reader.ReadResponse(530)
		requireTestCondition(t, err == nil, "AUTH not rejected before SASL decoding", err)
	}
	testResult(t)(fmt.Fprint(conn, "STARTTLS\r\n"))
	{
		_, _, err = reader.ReadResponse(220)
		requireTestCondition(t, err == nil, err)
	}
	testResult(t)(fmt.Fprint(conn, "EHLO plaintext-after-upgrade\r\n"))
	line, err := reader.ReadLine()
	requireTestConditionf(t, err != nil, "failed TLS resumed SMTP: %q", line)
}

func TestImplicitHandshakeAdmissionTimeoutAndShutdown(t *testing.T) {
	cfg := transportConfig(t, "tls")
	cfg.MaxConnections = 1
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	requireTestCondition(t, err == nil, err)
	reads := make(chan string, 3)
	server, address := serveTransportOn(t, cfg, &handshakeReadListener{Listener: listener, reads: reads})
	first, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(first.Close())
	}()
	waitSMTPConnections(t, server, 1, "handshake not tracked")
	second, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(second.Close())
	}()
	testCheck(t)(second.SetReadDeadline(time.Now().Add(time.Second)))
	b := make([]byte, 256)
	n, err := second.Read(b)
	requireTestConditionf(t, (n == 0 && err != nil), "implicit overload leaked plaintext %q %v", b[:n], err)
	testCheck(t)(first.SetReadDeadline(time.Now().Add(2 * time.Second)))
	{
		n, err = first.Read(b)
		requireTestConditionf(t, (n == 0 && err != nil), "stalled handshake not closed %q %v", b[:n], err)
	}
	waitSMTPConnections(t, server, 0, "handshake slot leaked")
	third, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(third.Close())
	}()
	waitSMTPHandshakeRead(t, reads, third)
	assertSMTPStalledShutdown(t, server)
	testCheck(t)(third.SetReadDeadline(time.Now().Add(time.Second)))
	n, err = third.Read(b)
	var transportErr net.Error
	requireTestConditionf(
		t,
		((n == 0 && err != nil) && (!errors.As(err, &transportErr) || !transportErr.Timeout())),
		"shutdown did not disconnect stalled handshake: bytes=%d error=%T: %v",
		n,
		err,
		err,
	)
}

func TestInvalidTransportConfiguration(t *testing.T) {
	for _, mode := range []string{"unknown", "starttls-required", "tls"} {
		cfg := transportConfig(t, "starttls")
		cfg.Transport = mode
		if mode == "tls" {
			cfg.TLSConfig = nil
		}
		{
			_, err := New(&receiptstore.Store{}, cfg)
			requireTestConditionf(t, err != nil, "invalid %s accepted", mode)
		}
	}
	cfg := transportConfig(t, "starttls")
	cfg.HandshakeTimeout = 100 * time.Millisecond
	{
		_, err := New(&receiptstore.Store{}, cfg)
		requireTestCondition(t, err != nil, "unbounded handshake configuration accepted")
	}
}

func TestImplicitTLSCertificateAndMinimumVersion(t *testing.T) {
	cfg := transportConfig(t, "tls")
	cfg.MaxConnections = 10
	cert, roots := relayCertificate(t)
	cfg.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	_, address := serveTransport(t, cfg)
	for _, tc := range []struct {
		name string
		cfg  *tls.Config
	}{
		{
			"wrong hostname",
			&tls.Config{
				RootCAs:    roots,
				ServerName: "wrong.example.test",
				MinVersion: tls.VersionTLS12,
			},
		},
		{
			"untrusted",
			&tls.Config{ServerName: "localhost", MinVersion: tls.VersionTLS12},
		},
		{"old TLS", obsoleteClientTLS(roots)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: time.Second}, Config: tc.cfg}).DialContext(
				t.Context(),
				"tcp",
				address,
			)
			if conn != nil {
				testCleanup(t)(conn.Close())
			}
			requireTestCondition(t, err != nil, "invalid TLS accepted")
		})
	}
}

// Protocol-only transport tests do not exercise durable grant publication.
// Real sender proofs and revocation are covered by sender_authority_test.go.
type protocolSenderAuthority struct{}

func (protocolSenderAuthority) AuthorizeSender(
	context.Context,
	receiptstore.ID,
	string,
) (*receiptstore.SenderAuthorization, error) {
	return &receiptstore.SenderAuthorization{Version: 1}, nil
}

// obsoleteClientTLS exercises rejection of clients limited to obsolete TLS.
func obsoleteClientTLS(roots *x509.CertPool) *tls.Config {
	// G402: negative handshake fixture; owner: FRY maintainers; expires: 2027-01-18.
	//nolint:gosec // The client must offer obsolete TLS to exercise rejection.
	return &tls.Config{
		RootCAs:    roots,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS10,
		MaxVersion: tls.VersionTLS11,
	}
}

// handshakeReadListener observes the actual handler boundary after go-smtp has
// registered its connection goroutine. Each accepted socket reports once.
type handshakeReadListener struct {
	net.Listener
	reads chan<- string
}

func (l *handshakeReadListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &handshakeReadConn{Conn: conn, reads: l.reads}, nil
}

type handshakeReadConn struct {
	net.Conn
	reads chan<- string
	once  sync.Once
}

func (c *handshakeReadConn) Read(p []byte) (int, error) {
	c.once.Do(func() { c.reads <- c.RemoteAddr().String() })
	return c.Conn.Read(p)
}

func waitSMTPConnections(t *testing.T, server *Server, want int, message string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		server.mu.Lock()
		n := len(server.connections)
		server.mu.Unlock()
		if n == want {
			return
		}
		requireTestCondition(t, !time.Now().After(deadline), message)
		time.Sleep(time.Millisecond)
	}
}

func waitSMTPHandshakeRead(t *testing.T, reads <-chan string, third net.Conn) {
	t.Helper()
	// A successful Dial only queues TCP admission. Wait for the protocol handler
	// to begin TLS reading so shutdown must drain a live, stalled handshake.
	ready := time.NewTimer(time.Second)
	defer ready.Stop()
waitingForRead:
	for {
		select {
		case peer := <-reads:
			if peer == third.LocalAddr().String() {
				break waitingForRead
			}
		case <-ready.C:
			t.Fatal("third TLS handshake did not start")
		}
	}
}

func submissionTLSClient(t *testing.T, address, mode string, roots *x509.CertPool) *smtp.Client {
	t.Helper()
	var client *smtp.Client
	var err error
	if mode == "tls" {
		conn, e := (&tls.Dialer{Config: &tls.Config{
			RootCAs:    roots,
			ServerName: "localhost",
			MinVersion: tls.VersionTLS12,
		}}).DialContext(
			t.Context(),
			"tcp",
			address,
		)
		requireTestCondition(t, e == nil, e)
		client, err = smtp.NewClient(conn, "localhost")
	} else {
		client, err = smtp.Dial(address)
		if err == nil {
			{
				supported, _ := client.Extension("AUTH")
				requireTestCondition(t, !supported, "AUTH advertised before TLS")
			}
			{
				err = client.Mail("user@example.test")
				requireTestCondition(t, err != nil, "plaintext submission accepted")
			}
			err = client.StartTLS(&tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12})
		}
	}
	requireTestCondition(t, err == nil, err)
	return client
}

func assertSMTPStalledShutdown(t *testing.T, server *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- server.Shutdown(ctx) }()
	select {
	case err := <-stopped:
		requireTestConditionf(
			t,
			errors.Is(err, context.DeadlineExceeded),
			"stalled handshake shutdown: want deadline exceeded, got %T: %v",
			err,
			err,
		)
	case <-time.After(time.Second):
		t.Fatal("shutdown blocked on handshake")
	}
	server.mu.Lock()
	remaining := len(server.connections)
	server.mu.Unlock()
	requireTestConditionf(t, remaining == 0, "shutdown leaked %d tracked connections", remaining)
}
