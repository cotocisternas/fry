package smtpserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/smtp"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/credentials"
	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/receiptstore"
)

func TestSubmissionEmptyLiveBootstrapAndStaticOverrideCompatibility(t *testing.T) {
	cfg := transportConfig(t, "starttls-required")
	cfg.Submission.Accounts = nil
	{
		_, err := New(&receiptstore.Store{}, cfg)
		requireTestCondition(t, err == nil, "emptylivebootstrap", err)
	}
	cfg.Submission.DefaultRelay = receiptstore.ID{}
	{
		_, err := New(&receiptstore.Store{}, cfg)
		requireTestCondition(t, err != nil, "emptybootstrap skippeddefaultrelayvalidation")
	}
	cfg.Transport = "loopback"
	cfg.TLSConfig = nil
	org := id(t)
	cfg.Submission.Credentials = nil
	cfg.Submission.Accounts = []SubmissionAccount{
		{Username: "static", Password: "static-secret", Organization: org, AuthorizedDomains: []string{"example.test"}},
	}
	cfg.Submission.OrganizationRelays = map[receiptstore.ID]receiptstore.ID{org: id(t)}
	{
		_, err := New(&receiptstore.Store{}, cfg)
		requireTestCondition(t, err == nil, "staticalloverridechanged", err)
	}
}

func liveClient(t *testing.T, f *fixture, user, password string) *smtp.Client {
	t.Helper()
	client, err := smtp.Dial(f.addr)
	requireTestCondition(t, err == nil, err)
	t.Cleanup(func() { testCleanup(t)(client.Close()) })
	roots := x509.NewCertPool()
	leaf, err := x509.ParseCertificate(f.config.TLSConfig.Certificates[0].Certificate[0])
	requireTestCondition(t, err == nil, err)
	roots.AddCert(leaf)
	{
		err = client.StartTLS(&tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12})
		requireTestCondition(t, err == nil, err)
	}
	{
		err = client.Auth(smtp.PlainAuth("", user, password, "127.0.0.1"))
		requireTestCondition(t, err == nil, err)
	}
	return client
}

func changeSubmissionPolicy(ctx context.Context, t *testing.T, f *fixture, user string, domains []string) {
	t.Helper()
	tx, err := f.pool.Begin(ctx)
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(tx.Rollback(ctx))
	}()
	{
		err = maildirectory.SetSubmissionPolicy(ctx, tx, f.org, user, nil, domains)
		requireTestCondition(t, err == nil, err)
	}
	{
		err = tx.Commit(ctx)
		requireTestCondition(t, err == nil, err)
	}
}

func TestSubmissionCreateAfterEmptyListenerAndPolicyFencing(t *testing.T) {
	f := setup(t, 1<<20)
	cert, _ := relayCertificate(t)
	directory := maildirectory.New(f.pool)
	f.config.Routes = nil
	f.config.Transport = "starttls-required"
	f.config.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	relay := id(t)
	f.config.Submission = &SubmissionConfig{
		Credentials:     directory,
		SenderAuthority: f.store,
		DefaultRelay:    relay,
		RetryWindow:     time.Hour,
	}
	f.start(t)
	encoded, err := credentials.HashPassword("live-created-secret")
	requireTestCondition(t, err == nil, err)
	tx, err := f.pool.Begin(context.Background())
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(tx.Rollback(context.Background()))
	}()
	{
		err = maildirectory.CreateSubmission(
			context.Background(),
			tx,
			maildirectory.SubmissionAccount{
				Username:          "live-user",
				Organization:      f.org.String(),
				Enabled:           true,
				AuthorizedDomains: []string{"business.test"},
			},
			encoded,
		)
		requireTestCondition(t, err == nil, err)
	}
	{
		err = tx.Commit(context.Background())
		requireTestCondition(t, err == nil, err)
	}
	requireLiveSubmissionPolicyFencing(t, f)
	var selected string
	{
		err = f.pool.QueryRow(context.Background(), `SELECT relay FROM fry_outbound`).Scan(&selected)
		requireTestCondition(t, (err == nil && selected == relay.String()), "newuserdefaultrelay", selected, err)
	}
}

func TestSubmissionPolicyNarrowingAfterAuthAndAtStreamEOF(t *testing.T) {
	for _, point := range []string{"AUTH", "EOF"} {
		t.Run(point, func(t *testing.T) {
			f, client := senderAuthorityFixture(t)
			changeSenderGrant(t.Context(), t, f, "business.test", true)
			if point == "AUTH" {
				changeSubmissionPolicy(t.Context(), t, f, "robot", []string{"new.test"})
				senderSMTPCode(t, client.Mail("robot@business.test"), 535)
				return
			}
			f.server.accept = func(
				ctx context.Context,
				attempt receiptstore.ID,
				e receiptstore.Envelope,
				r io.ReadCloser,
			) (
				receiptstore.Receipt,
				error,
			) {
				return f.store.Accept(
					ctx,
					attempt,
					e,
					&senderEOFAction{
						ReadCloser: r,
						action:     func() { changeSubmissionPolicy(ctx, t, f, "robot", []string{"new.test"}) },
					},
				)
			}
			{
				err := client.Mail("robot@business.test")
				requireTestCondition(t, err == nil, err)
			}
			senderSMTPCode(t, sendAuthorityBody(t, client), 535)
			var n int
			{
				err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM fry_receipt`).Scan(&n)
				requireTestCondition(t, (err == nil && n == 0), n, err)
			}
		})
	}
}

func requireLiveSubmissionPolicyFencing(t *testing.T, f *fixture) {
	t.Helper()
	var err error
	client := liveClient(t, f, "live-user", "live-created-secret")
	senderSMTPCode(t, client.Mail("robot@business.test"), 553) // credential scope cannot replace sender verification
	changeSenderGrant(t.Context(), t, f, "business.test", true)
	{
		err = client.Mail("robot@business.test")
		requireTestCondition(t, err == nil, err)
	}
	changeSubmissionPolicy(t.Context(), t, f, "live-user", []string{"new.test"})
	senderSMTPCode(t, sendAuthorityBody(t, client), 535)
	fresh := liveClient(t, f, "live-user", "live-created-secret")
	senderSMTPCode(t, fresh.Mail("robot@business.test"), 553)
	changeSenderGrant(t.Context(), t, f, "new.test", true)
	{
		err = fresh.Mail("robot@new.test")
		requireTestCondition(t, err == nil, err)
	}
	{
		err = sendAuthorityBody(t, fresh)
		requireTestCondition(t, err == nil, err)
	}
}
