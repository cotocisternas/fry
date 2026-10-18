package smtpserver

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/smtp"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/receiptstore"
)

func changeSenderGrant(ctx context.Context, t *testing.T, f *fixture, scope string, enable bool) {
	t.Helper()
	tx, err := f.pool.Begin(ctx)
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(tx.Rollback(ctx))
	}()
	if enable {
		_, err = receiptstore.OverrideSenderGrant(
			ctx,
			tx,
			f.org,
			scope,
			"global-operator",
			"synthetic integration approval",
		)
	} else {
		err = receiptstore.RevokeSenderGrant(
			ctx,
			tx,
			f.org,
			scope,
			"global-operator",
			"synthetic integration revocation",
		)
	}
	requireTestCondition(t, err == nil, err)
	{
		err = tx.Commit(ctx)
		requireTestCondition(t, err == nil, err)
	}
}

func senderAuthorityFixture(t *testing.T) (*fixture, *smtp.Client) {
	t.Helper()
	f := setup(t, 1<<20)
	cert, roots := relayCertificate(t)
	f.config.Routes = nil
	f.config.Transport = "starttls-required"
	f.config.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	directory := maildirectory.New(f.pool)
	{
		err := directory.SeedSubmission(
			context.Background(),
			[]maildirectory.SubmissionAccount{
				{
					Username:          "robot",
					Organization:      f.org.String(),
					Enabled:           true,
					AuthorizedDomains: []string{"business.test", "example.test"},
				},
			},
			[]string{"synthetic-password"},
		)
		requireTestCondition(t, err == nil, err)
	}
	f.config.Submission = &SubmissionConfig{
		Credentials:     directory,
		SenderAuthority: f.store,
		DefaultRelay:    id(t),
		RetryWindow:     time.Hour,
		Accounts: []SubmissionAccount{
			{
				Username:          "robot",
				Password:          "synthetic-password",
				Organization:      f.org,
				AuthorizedDomains: []string{"business.test"},
			},
		},
	}
	f.start(t)
	client, err := smtp.Dial(f.addr)
	requireTestCondition(t, err == nil, err)
	t.Cleanup(func() { testCleanup(t)(client.Close()) })
	{
		err = client.StartTLS(&tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12})
		requireTestCondition(t, err == nil, err)
	}
	{
		err = client.Auth(smtp.PlainAuth("", "robot", "synthetic-password", "127.0.0.1"))
		requireTestCondition(t, err == nil, err)
	}
	return f, client
}

func senderSMTPCode(t *testing.T, err error, code int) {
	t.Helper()
	var response *textproto.Error
	requireTestConditionf(t, (errors.As(err, &response) && response.Code == code), "want SMTP %d, got %v", code, err)
}

func sendAuthorityBody(t *testing.T, client *smtp.Client) error {
	t.Helper()
	{
		err := client.Rcpt("recipient@external.test")
		requireTestCondition(t, err == nil, err)
	}
	data, err := client.Data()
	requireTestCondition(t, err == nil, err)
	{
		_, err = io.WriteString(data, "From: robot@business.test\r\nSubject: proof integration\r\n\r\noriginal\r\n")
		requireTestCondition(t, err == nil, err)
	}
	return data.Close()
}

func TestSubmissionLiveSenderGrantAndRevocation(t *testing.T) {
	f, client := senderAuthorityFixture(t)
	senderSMTPCode(t, client.Mail("robot@business.test"), 553)
	changeSenderGrant(t.Context(), t, f, "business.test", true)
	{
		err := client.Mail("robot@business.test")
		requireTestCondition(t, err == nil, err)
	}
	{
		err := sendAuthorityBody(t, client)
		requireTestCondition(t, err == nil, "verified sender not accepted", err)
	}
	var attemptRaw string
	var envelopeRaw []byte
	{
		err := f.pool.QueryRow(context.Background(), `SELECT id,envelope FROM fry_attempt WHERE state='accepted'`).
			Scan(&attemptRaw, &envelopeRaw)
		requireTestCondition(t, err == nil, err)
	}
	var original receiptstore.Envelope
	{
		err := json.Unmarshal(envelopeRaw, &original)
		requireTestCondition(t, err == nil, err)
	}
	requireTestCondition(
		t,
		(original.Outbound != nil && original.Outbound.SenderAuthorization != nil),
		"accepted work omitted proof snapshot",
	)
	{
		err := client.Mail("robot@business.test")
		requireTestCondition(t, err == nil, err)
	}
	changeSenderGrant(t.Context(), t, f, "business.test", false)
	senderSMTPCode(t, sendAuthorityBody(t, client), 553)
	senderSMTPCode(t, client.Mail("robot@business.test"), 553)
	// A revocation blocks new work without changing an already accepted receipt.
	var attempt receiptstore.ID
	decoded, err := hex.DecodeString(attemptRaw)
	requireTestCondition(t, err == nil, err)
	copy(attempt[:], decoded)
	status, err := f.store.Status(context.Background(), f.org, attempt)
	requireTestCondition(t, (err == nil && status.Receipt != nil), status, err)
	replay, err := f.store.Accept(
		context.Background(),
		attempt,
		original,
		io.NopCloser(strings.NewReader("must not replace accepted content")),
	)
	requireTestCondition(
		t,
		(err == nil && replay.ID == status.Receipt.ID),
		"revocation invalidated accepted replay",
		replay,
		err,
	)
	var receipts int
	{
		err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM fry_receipt`).Scan(&receipts)
		requireTestCondition(t, (err == nil && receipts == 1), "revoked DATA accepted new mail", receipts, err)
	}
}

type senderEOFAction struct {
	io.ReadCloser
	once   sync.Once
	action func()
}

func (r *senderEOFAction) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err == io.EOF {
		r.once.Do(r.action)
	}
	return n, err
}

func TestSubmissionRevocationDuringDataCannotPublish(t *testing.T) {
	f, client := senderAuthorityFixture(t)
	changeSenderGrant(t.Context(), t, f, "business.test", true)
	f.server.accept = func(
		ctx context.Context,
		attempt receiptstore.ID,
		env receiptstore.Envelope,
		source io.ReadCloser,
	) (
		receiptstore.Receipt,
		error,
	) {
		return f.store.Accept(
			ctx,
			attempt,
			env,
			&senderEOFAction{
				ReadCloser: source,
				action:     func() { changeSenderGrant(ctx, t, f, "business.test", false) },
			},
		)
	}
	{
		err := client.Mail("robot@business.test")
		requireTestCondition(t, err == nil, err)
	}
	senderSMTPCode(t, sendAuthorityBody(t, client), 553)
	senderSMTPCode(t, client.Mail("robot@business.test"), 553)
	var receipts int
	{
		err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM fry_receipt`).Scan(&receipts)
		requireTestCondition(t, (err == nil && receipts == 0), "revocation lost publication race", receipts, err)
	}
	var aborted int
	{
		err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM fry_attempt WHERE state='aborted'`).
			Scan(&aborted)
		requireTestCondition(t, (err == nil && aborted == 1), "rejected work not terminal", aborted, err)
	}
}

type unavailableSenderAuthority struct{}

func (unavailableSenderAuthority) AuthorizeSender(
	context.Context,
	receiptstore.ID,
	string,
) (*receiptstore.SenderAuthorization, error) {
	return nil, io.ErrUnexpectedEOF
}

func TestSubmissionSenderAuthorityUnavailableIsTemporary(t *testing.T) {
	cfg := transportConfig(t, "tls")
	cfg.Submission.SenderAuthority = nil
	{
		_, err := New(&receiptstore.Store{}, cfg)
		requireTestCondition(t, err != nil, "public submission allowed missing live authority")
	}
	f, client := senderAuthorityFixture(t)
	f.server.config.Submission.SenderAuthority = unavailableSenderAuthority{}
	senderSMTPCode(t, client.Mail("robot@business.test"), 451)
}

func TestSubmissionSenderDenialResolvesAcceptedOrUnknownStatus(t *testing.T) {
	for _, accepted := range []bool{true, false} {
		name := "unknown"
		if accepted {
			name = "already accepted"
		}
		t.Run(name, func(t *testing.T) {
			f, client := senderAuthorityFixture(t)
			changeSenderGrant(t.Context(), t, f, "business.test", true)
			f.server.accept = deniedSenderAcceptance(t)
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
			requireTestCondition(t, (!accepted || err == nil), "accepted status produced negative reply", err)
			if !accepted {
				requireTestCondition(t, err != nil, "unknown status acknowledged")
				var response *textproto.Error
				requireTestCondition(
					t,
					(!errors.As(err, &response) || response.Code < 500),
					"unknown publication received definite sender rejection",
					err,
				)
			}
		})
	}
}

func deniedSenderAcceptance(
	t *testing.T,
) func(context.Context, receiptstore.ID, receiptstore.Envelope, io.ReadCloser) (receiptstore.Receipt, error) {
	t.Helper()
	accept := func(
		_ context.Context,
		_ receiptstore.ID,
		_ receiptstore.Envelope,
		source io.ReadCloser,
	) (
		receiptstore.Receipt,
		error,
	) {
		_, err := io.Copy(io.Discard, source)
		testCleanup(t)(source.Close())
		if err != nil {
			return receiptstore.Receipt{}, err
		}
		return receiptstore.Receipt{}, receiptstore.ErrSenderUnauthorized
	}
	return accept
}
