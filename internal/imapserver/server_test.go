//go:build linux

package imapserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	store      *receiptstore.Store
	pool       *pgxpool.Pool
	org, inbox receiptstore.ID
	server     *Server
	address    string
}

func testID(t *testing.T) receiptstore.ID {
	t.Helper()
	id, err := hotstore.NewID()
	requireTestCondition(t, err == nil, err)
	return id
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("FRY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("run scripts/tests/receipts.sh for database-backed IMAP tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	requireTestCondition(t, err == nil, err)
	t.Cleanup(admin.Close)
	schema := pgx.Identifier{"fry_imap_" + testID(t).String()}.Sanitize()
	{
		_, err = admin.Exec(ctx, "CREATE SCHEMA "+schema)
		requireTestCondition(t, err == nil, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	requireTestCondition(t, err == nil, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = strings.Trim(schema, "\"")
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	requireTestCondition(t, err == nil, err)
	t.Cleanup(pool.Close)
	{
		err = receiptstore.Migrate(ctx, pool)
		requireTestCondition(t, err == nil, err)
	}
	var members [2]*receiptstore.LocalMember
	for i := range members {
		m, e := receiptstore.OpenLocalMember(
			ctx,
			pool,
			testID(t),
			fmt.Sprint("local-", i),
			t.TempDir(),
			hotstore.Options{MaxMessageBytes: 256 << 20, MaxBytes: 1 << 30, MaxConcurrentAppends: 4},
		)
		requireTestCondition(t, e == nil, e)
		members[i] = m
		t.Cleanup(func() { testCleanup(t)(m.Close()) })
	}
	store, err := receiptstore.New(
		ctx,
		pool,
		members,
		receiptstore.Options{Lease: time.Second, RecoveryRetention: time.Hour},
	)
	requireTestCondition(t, err == nil, err)
	f := &fixture{store: store, pool: pool, org: testID(t), inbox: testID(t)}
	{
		err = store.EnsureInbox(ctx, f.org, f.inbox)
		requireTestCondition(t, err == nil, err)
	}
	f.start(t)
	return f
}

func (f *fixture) start(t *testing.T) {
	t.Helper()
	server, err := New(
		f.store,
		[]Account{{Username: "test", Password: "synthetic-only-password", Organization: f.org, Inbox: f.inbox}},
	)
	requireTestCondition(t, err == nil, err)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	requireTestCondition(t, err == nil, err)
	f.server = server
	f.address = listener.Addr().String()
	go func() {
		testCleanup(t)(server.Serve(listener))
	}()
	t.Cleanup(func() { testCleanup(t)(server.Close()) })
}

func (f *fixture) add(t *testing.T, body string) {
	t.Helper()
	ctx := context.Background()
	_, err := f.store.Accept(
		ctx,
		testID(t),
		receiptstore.Envelope{
			Organization: f.org,
			MailFrom:     "sender@example.net",
			Destinations: []receiptstore.Destination{{Inbox: f.inbox, Recipients: []string{"inbox@example.test"}}},
		},
		io.NopCloser(strings.NewReader(body)),
	)
	requireTestCondition(t, err == nil, err)
	{
		_, err = f.store.PublishMailboxes(ctx, 100)
		requireTestCondition(t, err == nil, err)
	}
}

func (f *fixture) client(t *testing.T) *imapclient.Client {
	t.Helper()
	c, err := imapclient.DialInsecure(f.address, nil)
	requireTestCondition(t, err == nil, err)
	t.Cleanup(func() { testCleanup(t)(c.Close()) })
	{
		err = c.Login("test", "synthetic-only-password").Wait()
		requireTestCondition(t, err == nil, err)
	}
	return c
}

const original = "From: sender@example.net\r\nSubject: Invoice ABC\r\n" +
	"Date: Tue, 13 Oct 2026 12:00:00 -0300\r\n\r\noriginal\x00body\r\n"

func TestIMAPRawPartialFlagsSearchAndRestart(t *testing.T) {
	f := setup(t)
	f.add(t, original)
	c := f.client(t)
	selected, err := c.Select("INBOX", nil).Wait()
	requireTestCondition(t, err == nil, selected, err)
	requireTestCondition(t, selected.NumMessages == 1, selected, err)
	requireTestCondition(t, selected.UIDNext == 2, selected, err)
	section := &imap.FetchItemBodySection{Peek: true}
	data, err := c.Fetch(
		imap.UIDSetNum(1),
		&imap.FetchOptions{
			UID:         true,
			Flags:       true,
			RFC822Size:  true,
			BodySection: []*imap.FetchItemBodySection{section},
		},
	).
		Collect()
	requireTestCondition(t, err == nil, data, err)
	requireTestCondition(t, len(data) == 1, data, err)
	requireTestCondition(t, string(data[0].FindBodySection(section)) == original, data, err)
	requireTestCondition(t, data[0].RFC822Size == int64(len(original)), data, err)
	requireTestCondition(t, len(data[0].Flags) == 0, data, err)
	requireIMAPPartialFlagsAndSearch(t, c)
	testCleanup(t)(c.Close())
	testCleanup(t)(f.server.Close())
	f.start(t)
	c = f.client(t)
	after, err := c.Select("INBOX", nil).Wait()
	requireTestCondition(t, err == nil, after, err)
	requireTestCondition(t, after.UIDValidity == selected.UIDValidity, after, err)
	requireTestCondition(t, after.UIDNext == 2, after, err)
	data, err = c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{Flags: true}).Collect()
	requireTestCondition(t, err == nil, data, err)
	requireTestCondition(t, len(data) == 1, data, err)
	requireTestCondition(t, reflect.DeepEqual(data[0].Flags, []imap.Flag{imap.FlagSeen}), data, err)
}

func TestIMAPSharedSequenceViewAndReadOnly(t *testing.T) {
	f := setup(t)
	f.add(t, original)
	f.add(t, strings.Replace(original, "ABC", "XYZ", 1))
	a, b := f.client(t), f.client(t)
	{
		_, err := a.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err := b.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err := a.Store(
			imap.UIDSetNum(1),
			&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}},
			nil,
		).
			Collect()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err := a.Expunge().Collect()
		requireTestCondition(t, err == nil, err)
	}
	// b has not received EXPUNGE yet. Old sequence 2 must still identify UID 2.
	data, err := b.Fetch(imap.SeqSetNum(2), &imap.FetchOptions{UID: true}).Collect()
	requireTestCondition(t, err == nil, "sequence shifted before notification", data, err)
	requireTestCondition(t, len(data) == 1, "sequence shifted before notification", data, err)
	requireTestCondition(t, data[0].UID == 2, "sequence shifted before notification", data, err)
	{
		err = b.Noop().Wait()
		requireTestCondition(t, err == nil, err)
	}
	data, err = b.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{UID: true}).Collect()
	requireTestCondition(t, err == nil, data, err)
	requireTestCondition(t, len(data) == 1, data, err)
	requireTestCondition(t, data[0].UID == 2, data, err)
	requireIMAPReadOnlySelection(t, f, b)
}

func TestIMAPHeaderLimitAndPartialPastEnd(t *testing.T) {
	f := setup(t)
	f.add(t, original)
	c := f.client(t)
	{
		_, err := c.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	section := &imap.FetchItemBodySection{Peek: true, Partial: &imap.SectionPartial{Offset: 1 << 60, Size: 1 << 60}}
	data, err := c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{section}}).
		Collect()
	requireTestCondition(t, err == nil, data, err)
	requireTestCondition(t, len(data) == 1, data, err)
	requireTestCondition(t, len(data[0].FindBodySection(section)) == 0, data, err)
	f.add(t, "Subject: "+strings.Repeat("x", maxHeaderBytes)+"\r\n\r\nbody")
	{
		err = c.Noop().Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err = c.UIDSearch(
			&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "xxx"}}},
			nil,
		).
			Wait()
		requireTestCondition(t, err != nil, "oversized header returned a successful incomplete search")
	}
}

func TestIMAPCopyMoveAndFolders(t *testing.T) {
	f := setup(t)
	f.add(t, original)
	c := f.client(t)
	{
		_, err := c.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	for _, name := range []string{"Archive", "Processed"} {
		{
			err := c.Create(name, nil).Wait()
			requireTestCondition(t, err == nil, err)
		}
	}
	copied, err := c.Copy(imap.UIDSetNum(1), "Archive").Wait()
	requireTestCondition(t, err == nil, copied, err)
	requireTestCondition(t, copied.UIDValidity != 0, copied, err)
	moved, err := c.Move(imap.UIDSetNum(1), "Processed").Wait()
	requireTestCondition(t, err == nil, moved, err)
	requireTestCondition(t, moved.UIDValidity != 0, moved, err)
	status, err := c.Status("INBOX", &imap.StatusOptions{NumMessages: true}).Wait()
	requireTestCondition(t, err == nil, status, err)
	requireTestCondition(t, *status.NumMessages == 0, status, err)
	{
		err = c.Rename("Processed", "Done", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err = c.Select("Done", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	section := &imap.FetchItemBodySection{Peek: true}
	data, err := c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{section}}).
		Collect()
	requireTestCondition(t, err == nil, data, err)
	requireTestCondition(t, len(data) == 1, data, err)
	requireTestCondition(t, string(data[0].FindBodySection(section)) == original, data, err)
	{
		err = c.Delete("Archive").Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		err = c.Delete("INBOX").Wait()
		requireTestCondition(t, err != nil, "deleted INBOX")
	}
}

type repeatedByte struct{}

func (repeatedByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestIMAPLargeMessageStreamsWithoutClientCollection(t *testing.T) {
	if testing.Short() {
		t.Skip("200 MiB streaming case")
	}
	f := setup(t)
	const bodySize = int64(200 << 20)
	const header = "Content-Type: application/octet-stream\r\n\r\n"
	size := bodySize + int64(len(header))
	expected := sha256.New()
	stream := io.TeeReader(
		io.MultiReader(strings.NewReader(header), io.LimitReader(repeatedByte{}, bodySize)),
		expected,
	)
	c := f.client(t)
	appendCommand := c.Append("INBOX", size, nil)
	{
		_, err := io.Copy(appendCommand, stream)
		requireTestCondition(t, err == nil, err)
	}
	{
		err := appendCommand.Close()
		requireTestCondition(t, err == nil, err)
	}
	_, err := appendCommand.Wait()
	requireTestCondition(t, err == nil, err)

	{
		_, err = c.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	command := c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{Peek: true}}})
	actual, count, literals := streamFetchDigest(t, command)
	requireTestCondition(t, literals == 1, "streamed bytes differ", literals, count)
	requireTestCondition(t, count == size, "streamed bytes differ", literals, count)
	requireTestCondition(t, bytes.Equal(actual.Sum(nil), expected.Sum(nil)), "streamed bytes differ", literals, count)
	section := &imap.FetchItemBodySection{
		Peek:    true,
		Part:    []int{1},
		Partial: &imap.SectionPartial{Offset: bodySize - 50, Size: 100},
	}
	data, err := c.Fetch(
		imap.UIDSetNum(1),
		&imap.FetchOptions{
			BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
			BodySection:   []*imap.FetchItemBodySection{section},
		},
	).
		Collect()
	requireTestCondition(t, err == nil, data, err)
	requireTestCondition(t, len(data) == 1, data, err)
	structure, ok := data[0].BodyStructure.(*imap.BodyStructureSinglePart)
	requireTestConditionf(t, ok, "unexpected body structure: %T", data[0].BodyStructure)
	requireTestCondition(t, structure.Size == uint32(bodySize), "large MIME span mismatch")
	requireTestCondition(
		t,
		string(data[0].FindBodySection(section)) == strings.Repeat("x", 50),
		"large MIME span mismatch",
	)
}

func TestIMAPFilteredHeadersPreserveFoldingAndApplyPartialAfterFiltering(t *testing.T) {
	f := setup(t)
	// End one long field exactly at a buffered-read fragment boundary. Its following
	// CRLF must not be mistaken for the blank line that terminates all headers.
	long := "X-Pad: " + strings.Repeat("a", 4096-len("X-Pad: ")) + "\r\n"
	body := long + "From: sender@example.test\r\nSubject: first\r\n\tcontinued\r\nX-Hide: secret\r\nSubjec" +
		"t: repeated\r\n\r\nbody bytes\r\n"
	f.add(t, body)
	c := f.client(t)
	{
		_, err := c.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	selected := "Subject: first\r\n\tcontinued\r\nSubject: repeated\r\n\r\n"
	cases := []headerSectionExpectation{
		{
			&imap.FetchItemBodySection{
				Specifier:    imap.PartSpecifierHeader,
				HeaderFields: []string{"SUBJECT"},
				Peek:         true,
			},
			selected,
		},
		{
			&imap.FetchItemBodySection{
				Specifier:    imap.PartSpecifierHeader,
				HeaderFields: []string{"Subject"},
				Peek:         true,
				Partial:      &imap.SectionPartial{Offset: 12, Size: 17},
			},
			selected[12:29],
		},
		{
			&imap.FetchItemBodySection{
				Specifier:    imap.PartSpecifierHeader,
				HeaderFields: []string{"Missing"},
				Peek:         true,
			},
			"\r\n",
		},
		{
			&imap.FetchItemBodySection{
				Specifier:       imap.PartSpecifierHeader,
				HeaderFieldsNot: []string{"x-pad", "X-HIDE"},
				Peek:            true,
			},
			"From: sender@example.test\r\n" + selected,
		},
		{&imap.FetchItemBodySection{Specifier: imap.PartSpecifierText, Peek: true}, "body bytes\r\n"},
	}
	requireIMAPHeaderSections(t, c, cases)
}

func streamFetchDigest(t *testing.T, command *imapclient.FetchCommand) (hash.Hash, int64, int) {
	t.Helper()
	actual := sha256.New()
	var count int64
	var literals int
	for message := command.Next(); message != nil; message = command.Next() {
		for item := message.Next(); item != nil; item = message.Next() {
			if body, ok := item.(imapclient.FetchItemDataBodySection); ok {
				literals++
				n, e := io.Copy(actual, body.Literal)
				requireTestCondition(t, e == nil, e)
				count += n
			}
		}
	}
	{
		err := command.Close()
		requireTestCondition(t, err == nil, err)
	}
	return actual, count, literals
}

func requireIMAPPartialFlagsAndSearch(t *testing.T, c *imapclient.Client) {
	t.Helper()
	var data []*imapclient.FetchMessageBuffer
	var err error
	partial := &imap.FetchItemBodySection{Peek: true, Partial: &imap.SectionPartial{Offset: 9, Size: 17}}
	data, err = c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{partial}}).
		Collect()
	requireTestCondition(t, err == nil, data, err)
	requireTestCondition(t, len(data) == 1, data, err)
	requireTestCondition(t, string(data[0].FindBodySection(partial)) == original[9:26], data, err)
	{
		_, err = c.Store(
			imap.UIDSetNum(1),
			&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}},
			nil,
		).
			Collect()
		requireTestCondition(t, err == nil, err)
	}
	search, err := c.UIDSearch(
		&imap.SearchCriteria{
			Header:     []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "invoice"}},
			Flag:       []imap.Flag{imap.FlagSeen},
			SentSince:  time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC),
			SentBefore: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
		},
		nil,
	).
		Wait()
	requireTestCondition(t, err == nil, search, err)
	requireTestCondition(t, reflect.DeepEqual(search.AllUIDs(), []imap.UID{1}), search, err)
	{
		_, err = c.UIDSearch(&imap.SearchCriteria{Body: []string{"original"}}, nil).Wait()
		requireTestCondition(t, err != nil, "unsupported criterion silently ignored")
	}
}

func requireIMAPReadOnlySelection(t *testing.T, f *fixture, b *imapclient.Client) {
	t.Helper()
	var err error
	{
		_, err = b.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err = b.Store(
			imap.UIDSetNum(2),
			&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}},
			nil,
		).
			Collect()
		requireTestCondition(t, err != nil, "EXAMINE allowed STORE")
	}
	{
		_, err = b.Fetch(imap.UIDSetNum(2), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}}).Collect()
		requireTestCondition(t, err == nil, err)
	}
	_, messages, err := f.store.Mailbox(context.Background(), f.org, f.inbox, "INBOX")
	requireTestCondition(t, err == nil, "EXAMINE changed flags", messages, err)
	requireTestCondition(t, len(messages) == 1, "EXAMINE changed flags", messages, err)
	requireTestCondition(t, len(messages[0].Flags) == 0, "EXAMINE changed flags", messages, err)
	{
		err = b.UnselectAndExpunge().Wait()
		requireTestCondition(t, err == nil, err)
	}
}

type headerSectionExpectation struct {
	section *imap.FetchItemBodySection
	want    string
}

func requireIMAPHeaderSections(t *testing.T, c *imapclient.Client, cases []headerSectionExpectation) {
	t.Helper()
	for _, tt := range cases {
		got, err := c.Fetch(
			imap.UIDSetNum(1),
			&imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{tt.section}},
		).
			Collect()
		requireTestCondition(t, err == nil, got, err)
		requireTestCondition(t, len(got) == 1, got, err)
		requireTestConditionf(
			t,
			string(got[0].FindBodySection(tt.section)) == tt.want,
			"unexpected filtered body: %q",
			got[0].FindBodySection(tt.section),
		)
	}
}
