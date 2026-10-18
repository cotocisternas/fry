//go:build linux

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func listenerCertificate(t *testing.T) (transportConfig, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	checkCommand(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "FRY synthetic listener"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	checkCommand(t, err)
	private, err := x509.MarshalPKCS8PrivateKey(key)
	checkCommand(t, err)
	dir := t.TempDir()
	c := transportConfig{
		Transport:               "tls",
		CertificateFile:         filepath.Join(dir, "cert.pem"),
		KeyFile:                 filepath.Join(dir, "key.pem"),
		HandshakeTimeoutSeconds: 1,
	}
	checkCommand(
		t,
		os.WriteFile(c.CertificateFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600),
	)
	checkCommand(t, os.WriteFile(c.KeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0o600))
	roots := x509.NewCertPool()
	cert, err := x509.ParseCertificate(der)
	checkCommand(t, err)
	roots.AddCert(cert)
	return c, roots
}

func TestListenerConfigurationRejectsUnsafeModes(t *testing.T) {
	good, _ := listenerCertificate(t)
	for _, tc := range []struct {
		name, role, address, mode string
		certificate               bool
		wantOK                    bool
	}{
		{"local default", "incoming", "127.0.0.1:2525", "", false, true},
		{"public cleartext", "incoming", "0.0.0.0:25", "", false, false},
		{"hostname local", "incoming", "localhost:2525", "", false, false},
		{"missing key", "incoming", "0.0.0.0:25", "starttls", false, false},
		{"opportunistic MX", "incoming", "0.0.0.0:25", "starttls", true, true},
		{"optional submission", "submission", "0.0.0.0:587", "starttls", true, false},
		{"required submission", "submission", "0.0.0.0:587", "starttls-required", true, true},
		{"implicit IMAP", "imap", "[::]:993", "tls", true, true},
		{"unsupported IMAP upgrade", "imap", "0.0.0.0:143", "starttls-required", true, false},
		{"HTTPS", "administration", "0.0.0.0:443", "tls", true, true},
		{"ambiguous local certificate", "incoming", "127.0.0.1:25", "loopback", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := transportConfig{Transport: tc.mode}
			if tc.certificate {
				c = good
				c.Transport = tc.mode
			}
			err := c.prepare(tc.address, tc.role)
			if (err == nil) != tc.wantOK {
				t.Fatalf("prepare=%v", err)
			}
		})
	}
	// The public JSON shape remains flat in every listener, including old examples.
	for _, file := range []string{"local-smtp.json", "local-mail.json", "local-admin.json", "local-collector.json"} {
		// G304: fixed repository fixtures; owner: FRY maintainers; expires: 2027-01-18.
		//nolint:gosec // Paths come from the fixed fixture list above.
		raw, err := os.ReadFile(
			filepath.Join("..", "..", "examples", file),
		)
		checkCommand(t, err)
		var cfg config
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		checkCommand(t, decoder.Decode(&cfg))
		if err = cfg.prepareListeners(); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	var cfg config
	checkCommand(
		t,
		json.Unmarshal(
			[]byte(`{"listen":"127.0.0.1:25","admin":{"listen":"127.0.0.1:8080","origin":"https://127.0.0.1:8080"}}`),
			&cfg,
		),
	)
	if err := cfg.prepareListeners(); err == nil {
		t.Fatal("HTTPS origin accepted on plaintext listener")
	}
}

func TestAdministrationHTTPSAndHandshakeCapacity(t *testing.T) {
	transport, roots := listenerCertificate(t)
	checkCommand(t, transport.prepare("127.0.0.1:0", "administration"))
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	checkCommand(t, err)
	var handled atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			t.Error("handler saw cleartext")
		}
		handled.Add(1)
		if _, err := io.WriteString(w, "encrypted"); err != nil {
			t.Error(err)
		}
	})
	failures := make(chan error, 1)
	stop := serveAdministration(
		context.Background(),
		listener,
		handler,
		&adminConfig{transportConfig: transport, MaxConnections: 8},
		failures,
	)
	defer stop()
	address := listener.Addr().String()
	// A silent peer must expire on the handshake budget. Capacity saturation
	// and its server-side release barrier are tested separately below: peer EOF
	// can precede admissionConn.Close returning its slot.
	requireStalledHandshakeClosure(t, address)
	requireEncryptedResponse(t, address, roots)
	requireRejectedTLSClients(t, address, roots)
	if handled.Load() != 1 {
		t.Fatalf("unexpected handler invocations %d", handled.Load())
	}
	select {
	case err := <-failures:
		t.Fatalf("HTTPS serve: %v", err)
	default:
	}
}

// pausedCloseListener makes the peer-EOF/admission-release gap deterministic.
// Only its first accepted socket pauses Close after closing the underlying TCP
// socket, before admissionConn.Close can release its capacity token.
type pausedCloseListener struct {
	net.Listener
	accepted         atomic.Int32
	entered, release chan struct{}
}
type pausedCloseConn struct {
	net.Conn
	once             sync.Once
	entered, release chan struct{}
}

func (l *pausedCloseListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if l.accepted.Add(1) == 1 {
		return &pausedCloseConn{Conn: conn, entered: l.entered, release: l.release}, nil
	}
	return conn, nil
}

func (c *pausedCloseConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.entered); <-c.release })
	return err
}

func TestAdministrationAdmissionCloseCompletion(t *testing.T) {
	raw, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	checkCommand(t, err)
	paused := &pausedCloseListener{
		Listener: raw,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	bounded := &admissionListener{Listener: paused, slots: make(chan struct{}, 1)}
	defer closeTest(t, bounded)
	release := sync.OnceFunc(func() { close(paused.release) })
	defer release()
	peer, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", raw.Addr().String())
	checkCommand(t, err)
	defer closeTest(t, peer)
	admitted, err := bounded.Accept()
	checkCommand(t, err)
	if len(bounded.slots) != 1 {
		t.Fatal("first connection was not admitted")
	}
	closed := make(chan error, 1)
	go func() { closed <- admitted.Close() }()
	requirePeerEOFDuringClose(t, peer, bounded, paused)
	next := make(chan admissionResult, 1)
	go func() { conn, err := bounded.Accept(); next <- admissionResult{conn, err} }()
	requireSaturatedAfterEOF(t, raw.Addr().String(), bounded, next)

	release()
	requireCloseCompleted(t, closed, bounded)
	requireRecoveredAdmission(t, raw.Addr().String(), bounded, next)
}

func closeTest(t *testing.T, c io.Closer) {
	t.Helper()
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Error(err)
	}
}

func requireStalledHandshakeClosure(t *testing.T, address string) {
	t.Helper()
	slow, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
	checkCommand(t, err)
	defer closeTest(t, slow)
	{
		_, err := slow.Write([]byte{22, 3, 3, 0, 100})
		checkCommand(t, err)
	}
	var b [1]byte
	checkCommand(t, slow.SetReadDeadline(time.Now().Add(2*time.Second)))
	if _, err = slow.Read(b[:]); err == nil {
		t.Fatal("stalled handshake remained open")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("client deadline expired before server closed stalled handshake")
	}
	closeTest(t, slow)
}

func requireEncryptedResponse(t *testing.T, address string, roots *x509.CertPool) {
	t.Helper()
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		},
	}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+address, nil)
	checkCommand(t, err)
	response, err := client.Do(request)
	checkCommand(t, err)
	body, err := io.ReadAll(response.Body)
	if err := response.Body.Close(); err != nil {
		t.Error(err)
	}
	if err != nil || string(body) != "encrypted" {
		t.Fatalf("body %q, %v", body, err)
	}
}

func requireRejectedTLSClients(t *testing.T, address string, roots *x509.CertPool) {
	t.Helper()
	for _, settings := range []*tls.Config{
		{MinVersion: tls.VersionTLS12},
		{RootCAs: roots, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11},
	} {
		c, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: time.Second}, Config: settings}).DialContext(
			t.Context(),
			"tcp",
			address,
		)
		if err == nil {
			closeTest(t, c)
			t.Fatal("untrusted or obsolete TLS accepted")
		}
	}
	plaintext, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
	checkCommand(t, err)
	checkCommand(t, plaintext.SetDeadline(time.Now().Add(time.Second)))
	{
		_, err := io.WriteString(plaintext, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
		checkCommand(t, err)
	}
	if _, err := io.ReadAll(plaintext); err != nil && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatal(err)
	}
	closeTest(t, plaintext)
}

type admissionResult struct {
	conn net.Conn
	err  error
}

func requirePeerEOFDuringClose(t *testing.T, peer net.Conn, bounded *admissionListener, paused *pausedCloseListener) {
	t.Helper()
	select {
	case <-paused.entered:
	case <-time.After(time.Second):
		t.Fatal("server close not reached")
	}
	checkCommand(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
	var b [1]byte
	if n, err := peer.Read(b[:]); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatal("peer did not observe TCP closure", n, err)
	}
	if len(bounded.slots) != 1 {
		t.Fatal("capacity released before server Close completed")
	}
}

func requireSaturatedAfterEOF(t *testing.T, address string, bounded *admissionListener, next <-chan admissionResult) {
	t.Helper()
	// This reproduces the original HTTPS EOF even though the first peer has
	// already observed EOF: its server-side Close still holds the sole slot.
	client := &http.Client{
		Timeout:   time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+address, nil)
	checkCommand(t, err)
	response, err := client.Do(request)
	if response != nil {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("full admission after peer EOF: %v", err)
	}
	if len(bounded.slots) != 1 {
		t.Fatal("rejection released the original admission slot")
	}
	select {
	case result := <-next:
		if result.conn != nil {
			closeTest(t, result.conn)
		}
		t.Fatalf("saturated listener admitted another connection: %v", result.err)
	default:
	}
}

func requireCloseCompleted(t *testing.T, closed <-chan error, bounded *admissionListener) {
	t.Helper()
	select {
	case err := <-closed:
		checkCommand(t, err)
	case <-time.After(time.Second):
		t.Fatal("server Close did not complete")
	}
	if len(bounded.slots) != 0 {
		t.Fatal("completed Close retained capacity")
	}
}

func requireRecoveredAdmission(t *testing.T, address string, bounded *admissionListener, next <-chan admissionResult) {
	t.Helper()
	recovered, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
	checkCommand(t, err)
	defer closeTest(t, recovered)
	select {
	case result := <-next:
		if result.err != nil {
			t.Fatal(result.err)
		}
		defer closeTest(t, result.conn)
		if len(bounded.slots) != 1 {
			t.Fatal("next connection not admitted after completed Close")
		}
		closeTest(t, result.conn)
	case <-time.After(time.Second):
		t.Fatal("admission did not recover")
	}
	if len(bounded.slots) != 0 {
		t.Fatal("recovered connection retained capacity")
	}
}
