package smtpserver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/outbound"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

type authenticatedRelay struct {
	authenticated bool
	count         *atomic.Int32
}

func (s *authenticatedRelay) Reset()                   {}
func (s *authenticatedRelay) Logout() error            { return nil }
func (s *authenticatedRelay) AuthMechanisms() []string { return []string{sasl.Plain} }
func (s *authenticatedRelay) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, user, password string) error {
		if identity != "" || user != "relay-user" || password != "relay-test-password" {
			return smtp.ErrAuthFailed
		}
		s.authenticated = true
		return nil
	}), nil
}

func (s *authenticatedRelay) Mail(string, *smtp.MailOptions) error {
	if !s.authenticated {
		return smtp.ErrAuthRequired
	}
	return nil
}
func (s *authenticatedRelay) Rcpt(string, *smtp.RcptOptions) error { return nil }
func (s *authenticatedRelay) Data(r io.Reader) error {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	s.count.Add(1)
	return nil
}

func relayCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	requireTestCondition(t, err == nil, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "synthetic relay"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	requireTestCondition(t, err == nil, err)
	cert, err := x509.ParseCertificate(der)
	requireTestCondition(t, err == nil, err)
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}, roots
}

func TestOutboundVerifiedTLSAuthenticationAndDeferredConfigFailure(t *testing.T) {
	for _, transport := range []string{"tls", "starttls"} {
		t.Run(transport, func(t *testing.T) {
			f := setup(t, 1<<20)
			cert, roots := relayCertificate(t)
			var count atomic.Int32
			server := smtp.NewServer(
				smtp.BackendFunc(
					func(*smtp.Conn) (smtp.Session, error) { return &authenticatedRelay{count: &count}, nil },
				),
			)
			server.Domain = "relay.local"
			server.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			requireTestCondition(t, err == nil, err)
			if transport == "tls" {
				listener = tls.NewListener(listener, server.TLSConfig)
			}
			go func() {
				testCleanup(t)(server.Serve(listener))
			}()
			defer func() {
				testCleanup(t)(server.Close())
			}()
			relay := id(t)
			config := outbound.Relay{
				ID:        relay,
				Address:   listener.Addr().String(),
				Transport: transport,
				Username:  "relay-user",
				Password:  "wrong-password",
				Roots:     roots,
			}
			requireRelayRetryAfterCredentialFix(t, f, config, &count)
		})
	}
}

func requireRelayRetryAfterCredentialFix(
	t *testing.T,
	f *fixture,
	config outbound.Relay,
	count *atomic.Int32,
) {
	t.Helper()
	ctx := context.Background()
	worker, err := outbound.New(f.store, "fry.local", []outbound.Relay{config})
	requireTestCondition(t, err == nil, err)
	receipt := submitQueued(t, f, config.ID, "recipient@example.net")
	{
		_, err = worker.RunOnce(ctx)
		requireTestCondition(t, err == nil, err)
	}
	states, err := f.store.OutboundState(ctx, f.org, receipt.ID)
	requireTestCondition(
		t,
		((err == nil && states["recipient@example.net"] == "queued") && count.Load() == 0),
		"configuration failure bounced valid mail",
		states,
		count.Load(),
		err,
	)
	config.Password = "relay-test-password"
	worker, err = outbound.New(f.store, "fry.local", []outbound.Relay{config})
	requireTestCondition(t, err == nil, err)
	{
		_, err = f.pool.Exec(ctx, `UPDATE fry_outbound SET available_at=clock_timestamp()-interval '1 second'`)
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err = worker.RunOnce(ctx)
		requireTestCondition(t, err == nil, err)
	}
	states, err = f.store.OutboundState(ctx, f.org, receipt.ID)
	requireTestCondition(
		t,
		((err == nil && states["recipient@example.net"] == "handed-off") && count.Load() == 1),
		states,
		count.Load(),
		err,
	)
}
