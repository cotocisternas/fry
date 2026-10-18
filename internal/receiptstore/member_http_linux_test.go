//go:build linux

package receiptstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"
)

type replicaTLS struct {
	server         *tls.Config
	allowed, other *http.Client
	fingerprint    [32]byte
}

func replicaCertificates(t *testing.T) replicaTLS {
	t.Helper()
	return replicaCertificatesUntil(t, time.Now().Add(time.Hour))
}

func replicaCertificatesUntil(t *testing.T, expiry time.Time) replicaTLS {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "FRY fixture CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              expiry,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	serverCert := makeReplicaCertificate(t, ca, root, caKey, 2, true)
	clientCert := makeReplicaCertificate(t, ca, root, caKey, 3, false)
	otherCert := makeReplicaCertificate(t, ca, root, caKey, 4, false)
	client := func(cert tls.Certificate) *http.Client {
		transport := &http.Transport{
			ForceAttemptHTTP2: true,
			TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS13,
				RootCAs:      roots,
				Certificates: []tls.Certificate{cert},
			},
			DisableCompression:    true,
			MaxConnsPerHost:       8,
			TLSHandshakeTimeout:   2 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
		}
		t.Cleanup(transport.CloseIdleConnections)
		return &http.Client{
			Transport:     transport,
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return replicaTLS{
		server:      &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: roots},
		allowed:     client(clientCert),
		other:       client(otherCert),
		fingerprint: sha256.Sum256(clientCert.Leaf.RawSubjectPublicKeyInfo),
	}
}

func memberHTTP(
	t *testing.T,
	f *fixture,
	wrappers ...func(http.Handler) http.Handler,
) (*MemberServer, replicaTLS, string) {
	t.Helper()
	certs := replicaCertificates(t)
	// Cancel startup immediately: future requests must retain their own lifetime.
	startup, cancelStartup := context.WithCancel(context.Background())
	server, err := NewMemberServer(
		startup,
		f.members[0],
		MemberServerOptions{
			TLS:             certs.server,
			CoordinatorKeys: [][32]byte{certs.fingerprint},
			MaxRequests:     8,
			Timeout:         30 * time.Second,
		},
	)
	cancelStartup()
	if err != nil {
		t.Fatal(err)
	}
	server.http.ErrorLog = log.New(io.Discard, "", 0)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	for _, wrap := range wrappers {
		server.http.Handler = legacyReplicaWrapper(wrap, server.http.Handler)
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return server, certs, "https://" + listener.Addr().String()
}

func replicaMetadata(c claim, s *MemberServer) memberRequest {
	return memberRequest{
		Attempt:      c.Attempt,
		Receipt:      c.Receipt,
		Organization: c.Organization,
		Epoch:        c.Epoch,
		Profile:      c.Profile,
		OwnerEpoch:   s.owner.epoch,
		Incarnation:  s.owner.member.bytes.FileID(),
	}
}

func receiptTestRequest(
	t *testing.T,
	client *http.Client,
	_ string,
	url string,
	body io.Reader,
) (*http.Response, error) {
	t.Helper()
	var metadata memberRequest
	if body != nil {
		if err := json.NewDecoder(body).Decode(&metadata); err != nil {
			return nil, err
		}
	}
	return legacyReplicaCall(t, client, url, metadata)
}

func makeReplicaCertificate(
	t *testing.T,
	ca, root *x509.Certificate,
	caKey *ecdsa.PrivateKey,
	serial int64,
	server bool,
) tls.Certificate {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		NotBefore:    ca.NotBefore,
		NotAfter:     ca.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if server {
		cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		cert.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, e := x509.CreateCertificate(rand.Reader, cert, root, &key.PublicKey, caKey)
	if e != nil {
		t.Fatal(e)
	}
	parsed, e := x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}
}
