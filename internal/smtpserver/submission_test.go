package smtpserver

import (
	"context"
	"encoding/hex"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/outbound"
	"github.com/cotocisternas/fry/internal/receiptstore"
)

func TestAuthenticatedSubmissionRequiresAuthorizedSenderAndSnapshotsRelay(t *testing.T) {
	f := setup(t, 1<<20)
	defaultRelay, override := id(t), id(t)
	f.config.Routes = nil
	f.config.Submission = &SubmissionConfig{
		DefaultRelay:       defaultRelay,
		OrganizationRelays: map[receiptstore.ID]receiptstore.ID{f.org: override},
		RetryWindow:        time.Hour,
		Accounts: []SubmissionAccount{
			{
				Username:          "robot",
				Password:          "synthetic-password",
				Organization:      f.org,
				AuthorizedDomains: []string{"business.test"},
			},
			{
				Username:          "second",
				Password:          "another-password",
				Organization:      f.other,
				AuthorizedSenders: []string{"sender@other.test"},
			},
		},
	}
	f.start(t)
	client, err := smtp.Dial(f.addr)
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(client.Close())
	}()
	requireAuthorizedSubmission(t, client)
	var relay, org string
	ctx := context.Background()
	{
		err = f.pool.QueryRow(
			ctx,
			`SELECT q.relay,r.organization FROM fry_outbound q JOIN fry_receipt r ON r.id=q.receipt`,
		).
			Scan(&relay, &org)
		requireTestCondition(t, ((err == nil && relay == override.String()) && org == f.org.String()), relay, org, err)
	}
	var count int
	{
		err = f.pool.QueryRow(ctx, `SELECT count(*) FROM fry_destination`).Scan(&count)
		requireTestCondition(t, (err == nil && count == 0), "submission leaked into inbound mailbox", count, err)
	}
	assertDefaultSubmissionRelay(t, f, defaultRelay)
}

func TestOutboundFailureNoticeIsDeliveredThroughSelectedRelay(t *testing.T) {
	source, target := setup(t, 1<<20), setup(t, 1<<20)
	target.config.Routes = append(
		target.config.Routes,
		Route{Address: "sender@example.net", Organization: target.org, Inboxes: target.config.Routes[0].Inboxes},
	)
	target.start(t)
	relay := id(t)
	ctx := context.Background()
	worker, err := outbound.New(
		source.store,
		"fry.local",
		[]outbound.Relay{{ID: relay, Address: target.addr, Transport: "loopback"}},
	)
	requireTestCondition(t, err == nil, err)
	submitQueued(t, source, relay, "unknown@example.test")
	{
		_, err = worker.RunOnce(ctx)
		requireTestCondition(t, err == nil, err)
	}
	{
		n, e := source.store.QueueFailureNotices(ctx, "fry.local", time.Hour, 10)
		requireTestCondition(t, (e == nil && n == 1), n, e)
	}
	{
		_, err = worker.RunOnce(ctx)
		requireTestCondition(t, err == nil, err)
	}
	var rawID string
	var size int64
	{
		err = target.pool.QueryRow(ctx, `SELECT id,raw_size FROM fry_receipt`).Scan(&rawID, &size)
		requireTestCondition(t, err == nil, err)
	}
	var receiptID receiptstore.ID
	decoded, err := hex.DecodeString(rawID)
	requireTestCondition(t, err == nil, err)
	copy(receiptID[:], decoded)
	var body strings.Builder
	{
		err = target.store.Read(ctx, target.org, receiptID, 0, size, &body)
		requireTestCondition(t, err == nil, err)
	}
	requireTestCondition(
		t,
		(strings.Contains(body.String(), "Action: failed\r\n") &&
			strings.Contains(
				body.String(),
				"Final-Recipient: rfc822; unknown@example.test\r\n",
			)),
		body.String(),
	)
	var from string
	{
		err = target.pool.QueryRow(ctx, `SELECT envelope->>'MailFrom' FROM fry_attempt`).Scan(&from)
		requireTestCondition(t, (err == nil && from == ""), "failure notice did not use null reverse path", from, err)
	}
}

func assertDefaultSubmissionRelay(t *testing.T, f *fixture, defaultRelay receiptstore.ID) {
	t.Helper()
	var err error
	var relay string
	ctx := context.Background()
	// A second organization uses the installation default, with its own sender.
	other, err := smtp.Dial(f.addr)
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(other.Close())
	}()
	{
		err = other.Auth(smtp.PlainAuth("", "second", "another-password", "127.0.0.1"))
		requireTestCondition(t, err == nil, err)
	}
	{
		err = other.Mail("sender@other.test")
		requireTestCondition(t, err == nil, err)
	}
	{
		err = other.Rcpt("external@example.net")
		requireTestCondition(t, err == nil, err)
	}
	data, err := other.Data()
	requireTestCondition(t, err == nil, err)
	testResult(t)(data.Write([]byte("Subject: second\r\n\r\nbody\r\n")))
	{
		err = data.Close()
		requireTestCondition(t, err == nil, err)
	}
	{
		err = f.pool.QueryRow(ctx, `SELECT q.relay FROM fry_outbound q JOIN fry_receipt r ON r.id=q.receipt WHERE `+
			`r.organization=$1`, f.other.String()).
			Scan(&relay)
		requireTestCondition(t, (err == nil && relay == defaultRelay.String()), relay, err)
	}
}

func requireAuthorizedSubmission(t *testing.T, client *smtp.Client) {
	t.Helper()
	var err error
	{
		err = client.Mail("robot@business.test")
		requireTestCondition(t, err != nil, "anonymous submission allowed")
	}
	{
		err = client.Auth(smtp.PlainAuth("", "robot", "synthetic-password", "127.0.0.1"))
		requireTestCondition(t, err == nil, err)
	}
	{
		err = client.Mail("sender@other.test")
		requireTestCondition(t, err != nil, "another organization's sender allowed")
	}
	{
		err = client.Mail("robot@business.test")
		requireTestCondition(t, err == nil, err)
	}
	{
		err = client.Rcpt("external@example.net")
		requireTestCondition(t, err == nil, err)
	}
	data, err := client.Data()
	requireTestCondition(t, err == nil, err)
	{
		_, err = data.Write([]byte("From: robot@business.test\r\nSubject: queued\r\n\r\noriginal\r\n"))
		requireTestCondition(t, err == nil, err)
	}
	{
		err = data.Close()
		requireTestCondition(t, err == nil, "receipt depended on absent relay", err)
	}
}
