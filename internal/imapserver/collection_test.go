//go:build linux

package imapserver

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/imapcollect"
	"github.com/cotocisternas/fry/internal/receiptstore"
)

func collection(t *testing.T, source, target *fixture, cleanup bool) *imapcollect.Collector {
	t.Helper()
	worker, err := imapcollect.New(
		target.store,
		imapcollect.Config{
			Connection:        testID(t),
			Organization:      target.org,
			Inbox:             target.inbox,
			Address:           source.address,
			Username:          "test",
			Password:          "synthetic-only-password",
			LoopbackPlaintext: true,
			Mappings:          []imapcollect.Mapping{{SourceFolder: "INBOX", DestinationFolder: "Collected"}},
			DeleteSource:      cleanup,
			MaxMessages:       100,
			MaxMessageBytes:   2 << 20,
			Timeout:           10 * time.Second,
		},
	)
	requireTestCondition(t, err == nil, err)
	return worker
}

func TestIMAPCollectionCopiesOnceAndDoesNotMirror(t *testing.T) {
	source, target := setup(t), setup(t)
	source.add(t, original)
	ctx := context.Background()
	{
		err := target.store.CreateFolder(ctx, target.org, target.inbox, "Collected")
		requireTestCondition(t, err == nil, err)
	}
	{
		err := source.store.ChangeFlags(
			ctx,
			source.org,
			source.inbox,
			"INBOX",
			[]uint32{1},
			"add",
			[]string{"\\Seen", "processed"},
		)
		requireTestCondition(t, err == nil, err)
	}
	worker := collection(t, source, target, false)
	result, err := worker.RunOnce(ctx)
	requireTestCondition(t, ((err == nil && result.Copied == 1) && result.Cleaned == 0), result, err)
	{
		_, err = target.store.PublishMailboxes(ctx, 100)
		requireTestCondition(t, err == nil, err)
	}
	_, mail, err := target.store.Mailbox(ctx, target.org, target.inbox, "Collected")
	requireTestCondition(
		t,
		((err == nil && len(mail) == 1) && reflect.DeepEqual(mail[0].Flags, []string{"\\Seen", "processed"})),
		mail,
		err,
	)
	requireCollectedOriginal(t, target, ctx, mail[0])
	requireCollectionDoesNotMirror(t, source, target, ctx, worker, mail)
}

func TestIMAPCollectionCleanupPreservesOtherDeletedMail(t *testing.T) {
	source, target := setup(t), setup(t)
	ctx := context.Background()
	source.add(t, original)
	source.add(t, original)
	{
		err := target.store.CreateFolder(ctx, target.org, target.inbox, "Collected")
		requireTestCondition(t, err == nil, err)
	}
	{
		err := source.store.ChangeFlags(
			ctx,
			source.org,
			source.inbox,
			"INBOX",
			[]uint32{2},
			"add",
			[]string{"\\Deleted"},
		)
		requireTestCondition(t, err == nil, err)
	}
	worker, err := imapcollect.New(
		target.store,
		imapcollect.Config{
			Connection:        testID(t),
			Organization:      target.org,
			Inbox:             target.inbox,
			Address:           source.address,
			Username:          "test",
			Password:          "synthetic-only-password",
			LoopbackPlaintext: true,
			Mappings:          []imapcollect.Mapping{{SourceFolder: "INBOX", DestinationFolder: "Collected"}},
			DeleteSource:      true,
			MaxMessages:       1,
			MaxMessageBytes:   2 << 20,
			Timeout:           10 * time.Second,
		},
	)
	requireTestCondition(t, err == nil, err)
	result, err := worker.RunOnce(ctx)
	requireTestCondition(t, ((err == nil && result.Copied == 1) && result.Cleaned == 1), result, err)
	_, remaining, err := source.store.Mailbox(ctx, source.org, source.inbox, "INBOX")
	requireTestCondition(
		t,
		((err == nil && len(remaining) == 1) && remaining[0].UID == 2),
		"cleanup expunged another deleted message",
		remaining,
		err,
	)
	var count int
	{
		err = target.pool.QueryRow(ctx, `SELECT count(*) FROM fry_receipt`).Scan(&count)
		requireTestCondition(t, (err == nil && count == 1), count, err)
	}
}

func TestIMAPCollectionDoesNotAcceptFailedFinalResponse(t *testing.T) {
	target := setup(t)
	ctx := context.Background()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	requireTestCondition(t, err == nil, err)
	defer func() {
		testCleanup(t)(listener.Close())
	}()
	done := serveFailedCollectionSource(t, listener)
	worker, err := imapcollect.New(
		target.store,
		imapcollect.Config{
			Connection:        testID(t),
			Organization:      target.org,
			Inbox:             target.inbox,
			Address:           listener.Addr().String(),
			Username:          "test",
			Password:          "synthetic",
			LoopbackPlaintext: true,
			Mappings:          []imapcollect.Mapping{{SourceFolder: "INBOX", DestinationFolder: "INBOX"}},
			MaxMessages:       10,
			MaxMessageBytes:   2 << 20,
			Timeout:           5 * time.Second,
		},
	)
	requireTestCondition(t, err == nil, err)
	{
		_, err = worker.RunOnce(ctx)
		requireTestCondition(t, err != nil, "failed source command accepted")
	}
	<-done
	var count int
	{
		err = target.pool.QueryRow(ctx, `SELECT count(*) FROM fry_receipt`).Scan(&count)
		requireTestCondition(t, (err == nil && count == 0), "failed response created receipt", count, err)
	}
	var sources int
	{
		err = target.pool.QueryRow(ctx, `SELECT count(*) FROM fry_source WHERE accepted`).Scan(&sources)
		requireTestCondition(t, (err == nil && sources == 0), sources, err)
	}
}

func serveFailedCollectionSource(t *testing.T, listener net.Listener) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer func() {
			testCleanup(t)(conn.Close())
		}()
		testCheck(t)(conn.SetDeadline(time.Now().Add(10 * time.Second)))
		r := bufio.NewReader(conn)
		testResult(t)(fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] synthetic source\r\n"))
		for {
			line, e := r.ReadString('\n')
			if e != nil {
				return
			}
			tag, command, _ := strings.Cut(strings.TrimSpace(line), " ")
			upper := strings.ToUpper(command)
			respondFailedCollectionCommand(t, conn, tag, upper)
		}
	}()
	return done
}

func respondFailedCollectionCommand(t *testing.T, conn net.Conn, tag, upper string) {
	t.Helper()
	switch {
	case strings.HasPrefix(upper, "LOGIN "):
		testResult(t)(fmt.Fprintf(conn, "%s OK logged in\r\n", tag))
	case strings.HasPrefix(upper, "EXAMINE "):
		testResult(t)(
			fmt.Fprintf(
				conn,
				"* 1 EXISTS\r\n* OK [UIDVALIDITY 77] valid\r\n* OK [UIDNEXT 2] next\r\n"+
					"* FLAGS ()\r\n%s OK [READ-ONLY] selected\r\n",
				tag,
			),
		)
	case strings.HasPrefix(upper, "UID SEARCH"):
		testResult(t)(fmt.Fprintf(conn, "* SEARCH 1\r\n%s OK searched\r\n", tag))
	case strings.HasPrefix(upper, "UID FETCH") && strings.Contains(upper, "RFC822.SIZE"):
		testResult(t)(
			fmt.Fprintf(
				conn,
				"* 1 FETCH (UID 1 FLAGS () INTERNALDATE \"13-Oct-2026 12:00:00 +0000\" "+
					"RFC822.SIZE %d)\r\n%s OK metadata\r\n",
				len(original),
				tag,
			),
		)
	case strings.HasPrefix(upper, "UID FETCH"):
		testResult(t)(
			fmt.Fprintf(
				conn,
				"* 1 FETCH (UID 1 BODY[] {%d}\r\n%s)\r\n%s NO source transfer failed\r\n",
				len(original),
				original,
				tag,
			),
		)
	default:
		testResult(t)(fmt.Fprintf(conn, "%s BAD unsupported\r\n", tag))
	}
}

func requireCollectedOriginal(t *testing.T, target *fixture, ctx context.Context, mail receiptstore.Mail) {
	t.Helper()
	var err error
	var bytes strings.Builder
	{
		err = target.store.Read(ctx, target.org, mail.Receipt, 0, mail.Size, &bytes)
		requireTestCondition(t, (err == nil && bytes.String() == original), bytes.String(), err)
	}
}

func requireCollectionDoesNotMirror(
	t *testing.T,
	source, target *fixture,
	ctx context.Context,
	worker *imapcollect.Collector,
	mail []receiptstore.Mail,
) {
	t.Helper()
	var result imapcollect.Result
	var err error
	result, err = worker.RunOnce(ctx)
	requireTestCondition(
		t,
		((err == nil && result.Copied == 0) && result.AlreadyPresent == 1),
		"duplicate collection",
		result,
		err,
	)
	{
		err = source.store.ChangeFlags(
			ctx,
			source.org,
			source.inbox,
			"INBOX",
			[]uint32{1},
			"set",
			[]string{"\\Deleted"},
		)
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err = source.store.Expunge(ctx, source.org, source.inbox, "INBOX", []uint32{1})
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err = worker.RunOnce(ctx)
		requireTestCondition(t, err == nil, err)
	}
	_, after, err := target.store.Mailbox(ctx, target.org, target.inbox, "Collected")
	requireTestCondition(
		t,
		((err == nil && len(after) == 1) && reflect.DeepEqual(after[0].Flags, mail[0].Flags)),
		"mirrored source state",
		after,
		err,
	)
}
