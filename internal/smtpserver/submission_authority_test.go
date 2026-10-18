package smtpserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/smtp"
	"testing"

	"github.com/cotocisternas/fry/internal/credentials"
	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/receiptstore"
)

type protocolCredentials struct{ org receiptstore.ID }

func (p protocolCredentials) AuthenticateSubmission(
	_ context.Context,
	user, password string,
) (maildirectory.SubmissionPrincipal, error) {
	if user != "user" || password != "synthetic-password" {
		return maildirectory.SubmissionPrincipal{}, maildirectory.ErrNotFound
	}
	return maildirectory.SubmissionPrincipal{
		Username:          user,
		Organization:      p.org,
		Version:           1,
		AuthorizedDomains: []string{"example.test"},
	}, nil
}

func (p protocolCredentials) ValidateSubmission(context.Context, maildirectory.SubmissionPrincipal) error {
	return nil
}

func mutateSubmission(ctx context.Context, t *testing.T, f *fixture, password string, enabled bool) {
	t.Helper()
	tx, err := f.pool.Begin(ctx)
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(tx.Rollback(ctx))
	}()
	if password != "" {
		hash, e := credentials.HashPassword(password)
		requireTestCondition(t, e == nil, e)
		err = maildirectory.RotateSubmission(ctx, tx, f.org, "robot", hash)
	} else {
		err = maildirectory.SetSubmissionAccess(ctx, tx, f.org, "robot", enabled)
	}
	requireTestCondition(t, err == nil, err)
	{
		err = tx.Commit(ctx)
		requireTestCondition(t, err == nil, err)
	}
}

func TestSubmissionCredentialStaleMailAndData(t *testing.T) {
	for _, atData := range []bool{false, true} {
		t.Run(map[bool]string{false: "MAIL", true: "DATA"}[atData], func(t *testing.T) {
			f, client := senderAuthorityFixture(t)
			changeSenderGrant(t.Context(), t, f, "business.test", true)
			if atData {
				{
					err := client.Mail("robot@business.test")
					requireTestCondition(t, err == nil, err)
				}
			}
			mutateSubmission(t.Context(), t, f, "", false)
			var err error
			if atData {
				err = sendAuthorityBody(t, client)
			} else {
				err = client.Mail("robot@business.test")
			}
			senderSMTPCode(t, err, 535)
			var count int
			{
				err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM fry_receipt`).Scan(&count)
				requireTestCondition(t, (err == nil && count == 0), count, err)
			}
		})
	}
}

func TestSubmissionCredentialDenialResolvesStatus(t *testing.T) {
	for _, accepted := range []bool{true, false} {
		t.Run(map[bool]string{true: "accepted", false: "unknown"}[accepted], func(t *testing.T) {
			f, client := senderAuthorityFixture(t)
			changeSenderGrant(t.Context(), t, f, "business.test", true)
			f.server.accept = func(
				_ context.Context,
				_ receiptstore.ID,
				_ receiptstore.Envelope,
				r io.ReadCloser,
			) (
				receiptstore.Receipt,
				error,
			) {
				_, err := io.Copy(io.Discard, r)
				testCleanup(t)(r.Close())
				if err != nil {
					return receiptstore.Receipt{}, err
				}
				return receiptstore.Receipt{}, receiptstore.ErrSubmissionUnauthorized
			}
			f.server.status = func(context.Context, receiptstore.ID, receiptstore.ID) (receiptstore.Status, error) {
				if accepted {
					return receiptstore.Status{State: "accepted"}, nil
				}
				return receiptstore.Status{}, io.ErrUnexpectedEOF
			}
			{
				err := client.Mail("robot@business.test")
				requireTestCondition(t, err == nil, err)
			}
			err := sendAuthorityBody(t, client)
			requireTestCondition(t, (!accepted || err == nil), err)
			requireTestCondition(t, (accepted || err != nil), "unknown accepted")
		})
	}
}

func TestSubmissionCredentialRotationNewAuthentication(t *testing.T) {
	f, client := senderAuthorityFixture(t)
	mutateSubmission(t.Context(), t, f, "replacement-password", true)
	senderSMTPCode(t, client.Mail("robot@business.test"), 535)
	directory := maildirectory.New(f.pool)
	{
		_, err := directory.AuthenticateSubmission(context.Background(), "robot", "synthetic-password")
		requireTestCondition(t, errors.Is(err, maildirectory.ErrNotFound), err)
	}
	fresh, err := smtp.Dial(f.addr)
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(fresh.Close())
	}()
	// Reuse the fixture's generated leaf as a private test trust anchor.
	roots := x509.NewCertPool()
	leaf, err := x509.ParseCertificate(f.config.TLSConfig.Certificates[0].Certificate[0])
	requireTestCondition(t, err == nil, err)
	roots.AddCert(leaf)
	{
		err = fresh.StartTLS(&tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12})
		requireTestCondition(t, err == nil, err)
	}
	{
		err = fresh.Auth(smtp.PlainAuth("", "robot", "replacement-password", "127.0.0.1"))
		requireTestCondition(t, err == nil, err)
	}
	p, err := directory.AuthenticateSubmission(context.Background(), "robot", "replacement-password")
	requireTestCondition(t, (err == nil && p.Version == 2), p, err)
}

func TestSubmissionCredentialDisableDuringStream(t *testing.T) {
	f, client := senderAuthorityFixture(t)
	changeSenderGrant(t.Context(), t, f, "business.test", true)
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
			&senderEOFAction{ReadCloser: r, action: func() { mutateSubmission(ctx, t, f, "", false) }},
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
}

func TestSubmissionCredentialSeedPreservesState(t *testing.T) {
	f := setup(t, 1<<20)
	ctx := context.Background()
	d := maildirectory.New(f.pool)
	accounts := []maildirectory.SubmissionAccount{
		{
			Username:          "robot",
			Organization:      f.org.String(),
			Enabled:           true,
			AuthorizedDomains: []string{"business.test", "example.test"},
		},
	}
	{
		err := d.SeedSubmission(ctx, accounts, []string{"initial-password"})
		requireTestCondition(t, err == nil, err)
	}
	mutateSubmission(t.Context(), t, f, "rotated-password", true)
	mutateSubmission(t.Context(), t, f, "", false)
	for _, secret := range []string{"", "stale-password"} {
		{
			err := d.SeedSubmission(ctx, accounts, []string{secret})
			requireTestCondition(t, err == nil, err)
		}
	}
	var enabled bool
	var version int64
	{
		err := f.pool.QueryRow(ctx, `SELECT enabled,version FROM fry_submission_account`).Scan(&enabled, &version)
		requireTestCondition(t, ((err == nil && !enabled) && version == 3), enabled, version, err)
	}
	accounts[0].Organization = f.other.String()
	{
		err := d.SeedSubmission(ctx, accounts, []string{""})
		requireTestCondition(t, errors.Is(err, maildirectory.ErrConflict), err)
	}
	accounts[0].Username = "missing"
	{
		err := d.SeedSubmission(ctx, accounts, []string{""})
		requireTestCondition(t, err != nil, "missing secret seeded")
	}
	mutateSubmission(t.Context(), t, f, "", true)
	{
		_, err := d.AuthenticateSubmission(ctx, "robot", "rotated-password")
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err := d.AuthenticateSubmission(ctx, "robot", "initial-password")
		requireTestCondition(t, errors.Is(err, maildirectory.ErrNotFound), err)
	}
}

type unavailableCredentials struct{}

func (unavailableCredentials) AuthenticateSubmission(
	context.Context,
	string,
	string,
) (maildirectory.SubmissionPrincipal, error) {
	return maildirectory.SubmissionPrincipal{}, io.ErrUnexpectedEOF
}

func (unavailableCredentials) ValidateSubmission(context.Context, maildirectory.SubmissionPrincipal) error {
	return io.ErrUnexpectedEOF
}

func TestSubmissionCredentialUnavailableTemporary(t *testing.T) {
	cfg := transportConfig(t, "tls")
	cfg.Submission.Credentials = nil
	{
		_, err := New(&receiptstore.Store{}, cfg)
		requireTestCondition(t, err != nil, "public static credential accepted")
	}
	f, client := senderAuthorityFixture(t)
	f.server.config.Submission.Credentials = unavailableCredentials{}
	senderSMTPCode(t, client.Mail("robot@business.test"), 451)
}

func TestSubmissionCredentialAuthOutageAndPolicyBinding(t *testing.T) {
	for _, mode := range []string{"outage", "organization", "username"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := senderAuthorityFixture(t)
			switch mode {
			case "outage":
				f.server.config.Submission.Credentials = unavailableCredentials{}
			case "organization":
				f.server.accounts = map[string]principal{"robot": {org: f.other}}
			case "username":
				delete(f.server.accounts, "robot")
			}
			fresh, err := smtp.Dial(f.addr)
			requireTestCondition(t, err == nil, err)
			defer func() {
				testCleanup(t)(fresh.Close())
			}()
			roots := x509.NewCertPool()
			leaf, err := x509.ParseCertificate(f.config.TLSConfig.Certificates[0].Certificate[0])
			requireTestCondition(t, err == nil, err)
			roots.AddCert(leaf)
			{
				err = fresh.StartTLS(&tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12})
				requireTestCondition(t, err == nil, err)
			}
			err = fresh.Auth(smtp.PlainAuth("", "robot", "synthetic-password", "127.0.0.1"))
			if mode == "outage" {
				senderSMTPCode(t, err, 451)
			} else {
				requireTestCondition(t, err == nil, "stale bootstrap policy overrode live account", err)
			}
		})
	}
}

func TestSubmissionCredentialBDATRevocation(t *testing.T) {
	for _, duringStream := range []bool{false, true} {
		t.Run(map[bool]string{false: "before DATA", true: "during stream"}[duringStream], func(t *testing.T) {
			f, client := senderAuthorityFixture(t)
			changeSenderGrant(t.Context(), t, f, "business.test", true)
			{
				err := client.Mail("robot@business.test")
				requireTestCondition(t, err == nil, err)
			}
			{
				err := client.Rcpt("recipient@external.test")
				requireTestCondition(t, err == nil, err)
			}
			if duringStream {
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
						&senderEOFAction{ReadCloser: r, action: func() { mutateSubmission(ctx, t, f, "", false) }},
					)
				}
			} else {
				mutateSubmission(t.Context(), t, f, "", false)
			}
			body := "Subject: chunked credential fencing\r\n\r\nsynthetic\r\n"
			{
				err := client.Text.PrintfLine("BDAT %d LAST", len(body))
				requireTestCondition(t, err == nil, err)
			}
			{
				_, err := client.Text.W.WriteString(body)
				requireTestCondition(t, err == nil, err)
			}
			{
				err := client.Text.W.Flush()
				requireTestCondition(t, err == nil, err)
			}
			{
				_, _, err := client.Text.ReadResponse(535)
				requireTestCondition(t, err == nil, err)
			}
			{
				err := client.Noop()
				requireTestCondition(t, err == nil, "framing not drained", err)
			}
			var n int
			{
				err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM fry_receipt`).Scan(&n)
				requireTestCondition(t, (err == nil && n == 0), n, err)
			}
		})
	}
}
