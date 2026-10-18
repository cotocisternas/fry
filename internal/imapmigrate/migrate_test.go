//go:build linux

package imapmigrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/imapserver"
	"github.com/cotocisternas/fry/internal/imapsource"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestFolderMapping(t *testing.T) {
	for _, tc := range []struct {
		name    string
		folders []imapsource.Folder
		want    string
		good    bool
	}{
		{
			"custom hierarchy",
			[]imapsource.Folder{{Name: "Projects.Work", Delimiter: '.', Subscribed: true}},
			"Projects/Work",
			true,
		},
		{"literal slash", []imapsource.Folder{{Name: "a/b", Delimiter: '.'}}, "", false},
		{"collision", []imapsource.Folder{{Name: "a.b", Delimiter: '.'}, {Name: "a/b", Delimiter: '/'}}, "", false},
		{"empty component", []imapsource.Folder{{Name: "a..b", Delimiter: '.'}}, "", false},
		{"inbox", []imapsource.Folder{{Name: "Inbox", Delimiter: '/'}}, "INBOX", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mapFolders(tc.folders)
			requireTestConditionf(t, (err == nil) == tc.good, "%+v %v", got, err)
			requireTestConditionf(
				t,
				(!tc.good ||
					((got[0].Destination == tc.want && got[0].Source == tc.folders[0].Name) &&
						got[0].Subscribed == tc.folders[0].Subscribed)),
				"%+v",
				got,
			)
		})
	}
}

type fixture struct {
	store      *receiptstore.Store
	pool       *pgxpool.Pool
	org, inbox receiptstore.ID
	server     *imapserver.Server
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
	members := openMigrationMembers(t, ctx, pool)
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
	server, err := imapserver.New(
		f.store,
		[]imapserver.Account{
			{Username: "test", Password: "synthetic-only-password", Organization: f.org, Inbox: f.inbox},
		},
	)
	requireTestCondition(t, err == nil, err)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	requireTestCondition(t, err == nil, err)
	f.server = server
	f.address = listener.Addr().String()
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
		if err := <-served; err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
}

func (f *fixture) client(t *testing.T) *imapclient.Client {
	t.Helper()
	c, err := imapclient.DialInsecure(f.address, nil)
	requireTestCondition(t, err == nil, err)
	t.Cleanup(func() {
		if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	{
		err = c.Login("test", "synthetic-only-password").Wait()
		requireTestCondition(t, err == nil, err)
	}
	return c
}

func appendMessage(t *testing.T, c *imapclient.Client, folder, body string, flags []imap.Flag) {
	t.Helper()
	cmd := c.Append(
		folder,
		int64(len(body)),
		&imap.AppendOptions{Flags: flags, Time: time.Date(2020, 2, 3, 4, 5, 6, 0, time.FixedZone("source", -3*3600))},
	)
	{
		_, err := io.Copy(cmd, strings.NewReader(body))
		requireTestCondition(t, err == nil, err)
	}
	{
		err := cmd.Close()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err := cmd.Wait()
		requireTestCondition(t, err == nil, err)
	}
}

type mailState struct {
	Body  string
	Date  time.Time
	Flags []string
}
type folderState struct {
	Subscribed bool
	Messages   []mailState
}

func readState(t *testing.T, c *imapclient.Client) map[string]folderState {
	t.Helper()
	folders, err := c.List("", "*", nil).Collect()
	requireTestCondition(t, err == nil, err)
	subs, err := c.LSub("", "*").Collect()
	requireTestCondition(t, err == nil, err)
	subscribed := map[string]bool{}
	for _, s := range subs {
		subscribed[s.Mailbox] = true
	}
	state := map[string]folderState{}
	for _, f := range folders {
		skip := false
		for _, a := range f.Attrs {
			if a == imap.MailboxAttrNoSelect {
				skip = true
			}
		}
		if skip {
			continue
		}
		current := readFolderState(t, c, f.Mailbox, subscribed[f.Mailbox])

		state[f.Mailbox] = current
	}
	return state
}

func migrator(t *testing.T, f *fixture, maxMessages int) *Migrator {
	t.Helper()
	m, err := New(
		f.store,
		Config{
			Organization: f.org,
			Job:          testID(t),
			Inbox:        testID(t),
			Source: imapsource.Config{
				Address:           f.address,
				Username:          "test",
				Password:          "synthetic-only-password",
				LoopbackPlaintext: true,
				MaxMessageBytes:   1 << 20,
			},
			MaxMessages: maxMessages,
			MaxWindows:  100,
			Timeout:     20 * time.Second,
		},
	)
	requireTestCondition(t, err == nil, err)
	return m
}

func runComplete(t *testing.T, m *Migrator, p *receiptstore.MigrationPass) Result {
	t.Helper()
	var total Result
	for range 30 {
		r, err := m.Run(context.Background(), p)
		requireTestCondition(t, err == nil, err)
		total.Copied += r.Copied
		total.AlreadyPresent += r.AlreadyPresent
		total.Removed += r.Removed
		if r.Complete {
			total.Complete = true
			return total
		}
		p, err = m.Resume(context.Background())
		requireTestCondition(t, err == nil, err)
	}
	t.Fatal("bounded migration did not complete")
	return total
}

func TestMigrationTCPReconciliationAndActivation(t *testing.T) {
	f := setup(t)
	c := f.client(t)
	before := prepareMigrationHistory(t, c)
	m := migrator(t, f, 100)
	pass, err := m.Begin(context.Background(), nil)
	requireTestCondition(t, err == nil, err)
	first := runComplete(t, m, pass)
	requireTestConditionf(t, first.Copied == 4, "copied %d", first.Copied)
	{
		got := readState(t, c)
		requireTestConditionf(t, reflect.DeepEqual(got, before), "source altered: %#v != %#v", got, before)
	}
	changeMigrationSourceHistory(t, c)
	expected := readState(t, c)
	final, err := m.Begin(
		context.Background(),
		&receiptstore.MigrationFreeze{
			Actor:          "test operator",
			Reason:         "integration test",
			Barrier:        "all synthetic writers stopped",
			ArrivalHandoff: "synthetic arrivals stopped",
			StartedAt:      time.Now(),
		},
	)
	requireTestCondition(t, err == nil, err)
	reconciled := runComplete(t, m, final)
	requireTestConditionf(t, reconciled.Removed >= 2, "expected stale memberships removed: %+v", reconciled)
	{
		err = final.Activate(context.Background())
		requireTestCondition(t, err == nil, err)
	}
	target := &fixture{store: f.store, org: f.org, inbox: m.config.Inbox}
	target.start(t)
	actual := readState(t, target.client(t))
	requireTestConditionf(
		t,
		reflect.DeepEqual(actual, expected),
		"destination mismatch\ngot: %#v\nwant: %#v",
		actual,
		expected,
	)
	{
		after := readState(t, c)
		requireTestCondition(t, reflect.DeepEqual(after, expected), "migration changed source")
	}
}

func TestMigrationBoundedResumeAndIdenticalMemberships(t *testing.T) {
	f := setup(t)
	c := f.client(t)
	raw := "Subject: identical\r\nMessage-ID: <same@example.test>\r\n\r\nidentical"
	for range 3 {
		appendMessage(t, c, "INBOX", raw, nil)
	}
	expected := readState(t, c)
	m := migrator(t, f, 1)
	p, err := m.Begin(context.Background(), nil)
	requireTestCondition(t, err == nil, err)
	one, err := m.Run(context.Background(), p)
	requireTestConditionf(t, ((err == nil && !one.Complete) && one.Copied == 1), "%+v %v", one, err)
	resumed, err := m.Resume(context.Background())
	requireTestCondition(t, err == nil, err)
	rest := runComplete(t, m, resumed)
	requireTestConditionf(t, rest.Copied == 2, "copied %+v", rest)
	// A second generation rereads metadata but reuses all durable placements.
	p, err = m.Begin(
		context.Background(),
		&receiptstore.MigrationFreeze{
			Actor:          "operator",
			Reason:         "test",
			Barrier:        "stopped",
			ArrivalHandoff: "stopped",
			StartedAt:      time.Now(),
		},
	)
	requireTestCondition(t, err == nil, err)
	final := runComplete(t, m, p)
	requireTestConditionf(t, (final.Copied == 0 && final.AlreadyPresent == 3), "duplicates recopied: %+v", final)
	{
		err = p.Activate(context.Background())
		requireTestCondition(t, err == nil, err)
	}
	target := &fixture{store: f.store, org: f.org, inbox: m.config.Inbox}
	target.start(t)
	got := readState(t, target.client(t))
	requireTestConditionf(t, reflect.DeepEqual(got, expected), "distinct identical memberships collapsed: %#v", got)
	requireTestCondition(t, bytes.Equal([]byte(got["INBOX"].Messages[0].Body), []byte(raw)), "bytes changed")
	requireTestCondition(t, reflect.DeepEqual(readState(t, c), expected), "source changed")
}

func readFolderState(t *testing.T, c *imapclient.Client, mailbox string, subscribed bool) folderState {
	t.Helper()
	{
		_, err := c.Select(mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
		requireTestCondition(t, err == nil, err)
	}
	search, err := c.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	requireTestCondition(t, err == nil, err)
	current := folderState{Subscribed: subscribed, Messages: []mailState{}}
	for _, uid := range search.AllUIDs() {
		item := readMessageState(t, c, uid)

		current.Messages = append(current.Messages, item)
	}
	sort.Slice(current.Messages, func(i, j int) bool { return current.Messages[i].Body < current.Messages[j].Body })
	return current
}

func readMessageState(t *testing.T, c *imapclient.Client, uid imap.UID) mailState {
	t.Helper()
	section := &imap.FetchItemBodySection{Peek: true}
	messages, err := c.Fetch(
		imap.UIDSetNum(uid),
		&imap.FetchOptions{
			UID:          true,
			Flags:        true,
			InternalDate: true,
			BodySection:  []*imap.FetchItemBodySection{section},
		},
	).
		Collect()
	requireTestConditionf(t, (err == nil && len(messages) == 1), "fetch %v %v", messages, err)
	msg := messages[0]
	item := mailState{Body: string(msg.FindBodySection(section)), Date: msg.InternalDate.UTC(), Flags: []string{}}
	for _, flag := range msg.Flags {
		if string(flag) != "\\Recent" {
			item.Flags = append(item.Flags, string(flag))
		}
	}
	sort.Strings(item.Flags)
	return item
}

func prepareMigrationHistory(t *testing.T, c *imapclient.Client) map[string]folderState {
	t.Helper()
	for _, folder := range []string{"Archive", "Empty", "DeletedFolder"} {
		{
			err := c.Create(folder, nil).Wait()
			requireTestCondition(t, err == nil, err)
		}
	}
	{
		err := c.Subscribe("Archive").Wait()
		requireTestCondition(t, err == nil, err)
	}
	raw := "From: sender@example.test\r\nSubject: Original\r\n\r\nbody\x00binary\r\n"
	appendMessage(t, c, "INBOX", raw, []imap.Flag{imap.FlagSeen, "old"})
	appendMessage(t, c, "INBOX", raw, []imap.Flag{imap.FlagDeleted})
	appendMessage(t, c, "Archive", "Subject: archived\r\n\r\narchive", nil)
	appendMessage(t, c, "DeletedFolder", "Subject: removed folder\r\n\r\nremoved", nil)
	before := readState(t, c)
	return before
}

func openMigrationMembers(t *testing.T, ctx context.Context, pool *pgxpool.Pool) [2]*receiptstore.LocalMember {
	t.Helper()
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
		t.Cleanup(func() {
			if err := m.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	return members
}

func changeMigrationSourceHistory(t *testing.T, c *imapclient.Client) {
	t.Helper()
	var err error
	// Exercise removals and additions on a surviving membership, physical expunge,
	// new arrivals, and source folder identity changes between generations.
	{
		_, err = c.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err = c.Store(
			imap.UIDSetNum(1),
			&imap.StoreFlags{
				Op:    imap.StoreFlagsSet,
				Flags: []imap.Flag{imap.FlagFlagged, "new"},
			},
			nil,
		).
			Collect()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err = c.UIDExpunge(imap.UIDSetNum(2)).Collect()
		requireTestCondition(t, err == nil, err)
	}
	appendMessage(t, c, "INBOX", "Subject: arrival\r\n\r\nnew", []imap.Flag{imap.FlagDeleted})
	{
		err = c.Rename("Archive", "Renamed", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		err = c.Delete("DeletedFolder").Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		err = c.Create("NewEmpty", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		err = c.Subscribe("Renamed").Wait()
		requireTestCondition(t, err == nil, err)
	}
}
