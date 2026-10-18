//go:build linux

package imapserver

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/emersion/go-imap/v2"
)

func TestIMAPCapturedHeaderSearchDoesNotOpenOriginal(t *testing.T) {
	f := setup(t)
	f.add(t, "Subject: eager private\r\n\r\nbody must stay out of metadata")
	ctx := context.Background()
	{
		_, err := f.pool.Exec(
			ctx,
			`CREATE TABLE eager_header_read_audit(n bigint NOT NULL); INSERT INTO `+
				`eager_header_read_audit VALUES(0); CREATE FUNCTION count_eager_header_read() `+
				`RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE eager_header_read_audit SET `+
				`n=n+1; RETURN NEW; END $$; CREATE TRIGGER count_eager_header_read AFTER INSERT `+
				`ON fry_read_pin FOR EACH ROW EXECUTE FUNCTION count_eager_header_read()`,
		)
		requireTestCondition(t, err == nil, err)
	}
	client := f.client(t)
	{
		_, err := client.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	data, err := client.UIDSearch(
		&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "eager private"}}},
		nil,
	).
		Wait()
	requireTestCondition(t, (err == nil && len(data.AllUIDs()) == 1), data, err)
	var opens int
	var header []byte
	{
		err = f.pool.QueryRow(ctx, `SELECT n FROM eager_header_read_audit`).Scan(&opens)
		requireTestCondition(t, (err == nil && opens == 0), "eager header search opened original", opens, err)
	}
	{
		err = f.pool.QueryRow(ctx, `SELECT raw_header FROM fry_header_metadata`).Scan(&header)
		requireTestCondition(
			t,
			(err == nil && string(header) == "Subject: eager private\r\n\r\n"),
			"capture privacy",
			string(header),
			err,
		)
	}
}

func TestIMAPRepeatedSearchDoesNotReopenHeaderBytes(t *testing.T) {
	f := setup(t)
	for range 8 {
		f.add(t, "Subject: =?utf-8?Q?Fatura_n=C3=BAmero?=\r\n"+
			"Date: Tue, 13 Oct 2026 23:00:00 -0300\r\n\r\nbody")
	}
	ctx := context.Background()
	prepareLazyHeaderReadAudit(t, f, ctx)
	query := &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "NÚMERO"}}}
	client := f.client(t)
	{
		_, err := client.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	first, err := client.UIDSearch(query, nil).Wait()
	requireTestCondition(t, (err == nil && len(first.AllUIDs()) == 8), first, err)
	var cold int64
	{
		err = f.pool.QueryRow(ctx, `SELECT n FROM header_read_audit`).Scan(&cold)
		requireTestCondition(t, (err == nil && cold == 8), "unexpected cold header read count", cold, err)
	}
	// Another authenticated session shares immutable headers, not membership or
	// decoded search maps. Selection and organization authorization still run.
	second := f.client(t)
	{
		_, err = second.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	again, err := second.UIDSearch(query, nil).Wait()
	requireTestCondition(t, (err == nil && len(again.AllUIDs()) == 8), again, err)
	var warm int64
	{
		err = f.pool.QueryRow(ctx, `SELECT n FROM header_read_audit`).Scan(&warm)
		requireTestCondition(
			t,
			(err == nil && warm == cold),
			"repeated Search reopened retained header bytes",
			cold,
			warm,
			err,
		)
	}
	{
		err = f.server.Close()
		requireTestCondition(t, err == nil, err)
	}
	f.start(t)
	restarted := f.client(t)
	{
		_, err = restarted.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	data, err := restarted.UIDSearch(query, nil).Wait()
	requireTestCondition(t, (err == nil && len(data.AllUIDs()) == 8), data, err)
	{
		err = f.pool.QueryRow(ctx, `SELECT n FROM header_read_audit`).Scan(&warm)
		requireTestCondition(t, (err == nil && warm == cold), "server restart lost persisted headers", cold, warm, err)
	}
}

func TestIMAPHeaderMetadataPreservesDateDecodingAndMalformedFailures(t *testing.T) {
	f := setup(t)
	f.add(
		t,
		"Subject: =?utf-8?Q?value_=3D=3Futf-8=3FQ=3Fmutated=3F=3D?=\r\n"+
			"Date: Tue, 13 Oct 2026 23:30:00 -0300\r\nX-Other: =?x-unknown?Q?bad?=\r\n\r\n"+
			"body",
	)
	client := f.client(t)
	{
		_, err := client.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	// A second decode of an already-decoded map would change the nested encoded
	// word. Each persisted metadata hit must parse a fresh map from the original header bytes.
	query := &imap.SearchCriteria{
		Header:     []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "=?utf-8?Q?mutated?="}},
		SentSince:  time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC),
		SentBefore: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
	}
	for range 2 {
		data, err := client.UIDSearch(query, nil).Wait()
		requireTestCondition(
			t,
			(err == nil && len(data.AllUIDs()) == 1),
			"cached decoding or sent-date semantics changed",
			data,
			err,
		)
	}
	{
		_, err := client.UIDSearch(
			&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "X-Other", Value: "bad"}}},
			nil,
		).
			Wait()
		requireTestCondition(t, err != nil, "cached unknown charset became searchable")
	}
	{
		data, err := client.UIDSearch(query, nil).Wait()
		requireTestCondition(
			t,
			(err == nil && len(data.AllUIDs()) == 1),
			"failed decode poisoned cached map",
			data,
			err,
		)
	}
	requireCachedHeaderAuthorization(t, f, client, query)
	{
		err := f.server.Close()
		requireTestCondition(t, err == nil, err)
	}
}

func TestIMAPHeaderMetadataDoesNotCacheOpenOrParsingErrors(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	session := &session{server: f.server, account: credential{org: f.org}}
	{
		_, _, err := session.headers(ctx, receiptstore.Mail{Receipt: testID(t), Size: 100})
		requireTestCondition(t, err != nil, "missing receipt unexpectedly opened")
	}
	var count int
	{
		err := f.pool.QueryRow(ctx, `SELECT count(*) FROM fry_header_metadata`).Scan(&count)
		requireTestCondition(t, err == nil, err)
	}
	requireTestCondition(t, count == 0, "failed open cached")
	f.add(t, "not a valid header\r\n\r\nbody")
	client := f.client(t)
	{
		_, err := client.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	for range 2 {
		{
			_, err := client.UIDSearch(
				&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "any"}}},
				nil,
			).
				Wait()
			requireTestCondition(t, err != nil, "malformed header became false-negative successful SEARCH")
		}
	}
	{
		err := f.pool.QueryRow(ctx, `SELECT count(*) FROM fry_header_metadata`).Scan(&count)
		requireTestCondition(t, err == nil, err)
	}
	requireTestCondition(t, count == 0, "parse failure cached")
}

func TestIMAPHeaderMetadataCannotReopenRemovedReceipt(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.add(t, "Subject: sensitive original\r\n\r\nbody")
	_, messages, err := f.store.Mailbox(ctx, f.org, f.inbox, "INBOX")
	requireTestCondition(t, (err == nil && len(messages) == 1), messages, err)
	stale := messages[0]
	session := &session{server: f.server, account: credential{org: f.org}}
	{
		_, _, err = session.headers(ctx, stale)
		requireTestCondition(t, err == nil, err)
	}
	{
		err = f.store.ChangeFlags(ctx, f.org, f.inbox, "INBOX", []uint32{stale.UID}, "add", []string{"\\Deleted"})
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err = f.store.Expunge(ctx, f.org, f.inbox, "INBOX", []uint32{stale.UID})
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err = f.pool.Exec(
			ctx,
			`UPDATE fry_reference SET expires_at=clock_timestamp()-interval '1 day' WHERE receipt=$1`,
			stale.Receipt.String(),
		)
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err = f.store.SweepExpiry(ctx, 100)
		requireTestCondition(t, err == nil, err)
	}
	reclaimHeaderReceiptMembers(t, f, ctx)
	{
		n, err := f.store.FinishCleanup(ctx, 100)
		requireTestCondition(t, (err == nil && n == 1), "metadata cleanup failed", n, err)
	}
	{
		_, _, err = session.headers(ctx, stale)
		requireTestCondition(
			t,
			errors.Is(err, receiptstore.ErrExpired),
			"stale Search/Fetch snapshot reopened removed persisted content",
			err,
		)
	}
}

func prepareLazyHeaderReadAudit(t *testing.T, f *fixture, ctx context.Context) {
	t.Helper()
	// Model retained originals accepted before eager capture, or while its
	// optional budget was exhausted. Preserve the cold lazy-open regression.
	{
		_, err := f.pool.Exec(ctx, `DELETE FROM fry_header_metadata`)
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err := f.pool.Exec(
			ctx,
			`CREATE TABLE header_read_audit(n bigint NOT NULL); INSERT INTO `+
				`header_read_audit VALUES(0); CREATE FUNCTION count_header_read() RETURNS `+
				`trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE header_read_audit SET n=n+1; RETURN `+
				`NEW; END $$; CREATE TRIGGER count_header_read AFTER INSERT ON fry_read_pin FOR `+
				`EACH ROW EXECUTE FUNCTION count_header_read()`,
		)
		requireTestCondition(t, err == nil, err)
	}
}

func requireCachedHeaderAuthorization(t *testing.T, f *fixture, client *imapclient.Client, query *imap.SearchCriteria) {
	t.Helper()
	var entries int
	{
		err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM fry_header_metadata`).Scan(&entries)
		requireTestCondition(t, err == nil, err)
	}
	requireTestCondition(t, entries == 1, "valid header not cached")
	_, messages, err := f.store.Mailbox(context.Background(), f.org, f.inbox, "INBOX")
	requireTestCondition(t, (err == nil && len(messages) == 1), messages, err)
	outsider := &session{server: f.server, account: credential{org: testID(t)}}
	{
		_, _, err := outsider.headers(context.Background(), messages[0])
		requireTestCondition(
			t,
			errors.Is(err, receiptstore.ErrNotFound),
			"cached receipt leaked across organizations",
			err,
		)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	direct := &session{server: f.server, account: credential{org: f.org}}
	{
		_, _, err = direct.headers(canceled, messages[0])
		requireTestCondition(
			t,
			errors.Is(err, context.Canceled),
			"persisted header ignored canceled authorization context",
			err,
		)
	}
	{
		err = f.store.ChangeFlags(
			context.Background(),
			f.org,
			f.inbox,
			"INBOX",
			[]uint32{1},
			"add",
			[]string{"\\Deleted"},
		)
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err = f.store.Expunge(context.Background(), f.org, f.inbox, "INBOX", []uint32{1})
		requireTestCondition(t, err == nil, err)
	}
	{
		data, err := client.UIDSearch(query, nil).Wait()
		requireTestCondition(
			t,
			(err == nil && len(data.AllUIDs()) == 0),
			"persisted header bypassed current membership",
			data,
			err,
		)
	}
}

func reclaimHeaderReceiptMembers(t *testing.T, f *fixture, ctx context.Context) {
	t.Helper()
	rows, err := f.pool.Query(ctx, `SELECT id FROM fry_member`)
	requireTestCondition(t, err == nil, err)
	var members []receiptstore.ID
	for rows.Next() {
		var value string
		{
			err = rows.Scan(&value)
			requireTestCondition(t, err == nil, err)
		}
		decoded, e := hex.DecodeString(value)
		requireTestCondition(t, e == nil, e)
		var member receiptstore.ID
		copy(member[:], decoded)
		members = append(members, member)
	}
	rowErr := rows.Err()
	rows.Close()
	requireTestCondition(t, rowErr == nil, rowErr)
	for _, member := range members {
		{
			err = f.store.ReclaimHot(ctx, member)
			requireTestCondition(t, err == nil, err)
		}
	}
}
