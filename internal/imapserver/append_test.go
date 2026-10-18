//go:build linux

package imapserver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

func appendMessage(
	t *testing.T,
	f *fixture,
	mailbox, body string,
	options *imap.AppendOptions,
) (*imap.AppendData, error) {
	t.Helper()
	client := f.client(t)
	command := client.Append(mailbox, int64(len(body)), options)
	_, err := io.WriteString(command, body)
	if err != nil {
		return nil, err
	}
	if err = command.Close(); err != nil {
		return nil, err
	}
	return command.Wait()
}

func TestIMAPAppendAtomicMembershipFlagsDateAndExpungedAcknowledgment(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.FixedZone("test", -3*3600))
	result, err := appendMessage(
		t,
		f,
		"INBOX",
		original,
		&imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen, imap.FlagFlagged}, Time: when},
	)
	requireTestCondition(t, err == nil, err)
	folder, messages, err := f.store.Mailbox(ctx, f.org, f.inbox, "INBOX")
	requireTestCondition(t, (err == nil && len(messages) == 1), messages, err)
	m := messages[0]
	requireTestCondition(
		t,
		((((uint32(result.UID) == m.UID && result.UIDValidity == folder.UIDValidity) &&
			m.Date.Equal(when)) &&
			hasFlag(m, "\\Seen")) &&
			hasFlag(m, "\\Flagged")),
		result,
		folder,
		m,
	)
	var pending int
	{
		err = f.pool.QueryRow(ctx, `SELECT count(*) FROM fry_work`).Scan(&pending)
		requireTestCondition(t, (err == nil && pending == 0), "APPEND left delayed publication work", pending, err)
	}
	{
		err = f.store.ChangeFlags(ctx, f.org, f.inbox, "INBOX", []uint32{m.UID}, "add", []string{"\\Deleted"})
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err = f.store.Expunge(ctx, f.org, f.inbox, "INBOX", nil)
		requireTestCondition(t, err == nil, err)
	}
	placement, err := f.store.Appended(ctx, f.org, m.Receipt)
	requireTestCondition(
		t,
		(err == nil && placement.UID == m.UID),
		"acknowledgment vanished on expunge",
		placement,
		err,
	)
	// EXAMINE protects selected-state mutations, not an explicitly named APPEND.
	c := f.client(t)
	{
		_, err = c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
		requireTestCondition(t, err == nil, err)
	}
	command := c.Append("INBOX", 0, nil)
	{
		err = command.Close()
		requireTestCondition(t, err == nil, err)
	}
	empty, err := command.Wait()
	requireTestCondition(t, (err == nil && empty.UID == 2), empty, err)
}

func TestIMAPAppendFailureDoesNotPublishLaterOrConsumeUID(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// Force failure after mailbox insertion but before receipt acceptance commits.
	_, err := f.pool.Exec(
		ctx,
		`CREATE FUNCTION reject_append() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN `+
			`RAISE EXCEPTION 'fixture failure'; END $$; CREATE TRIGGER reject_append BEFORE `+
			`INSERT ON fry_append_result FOR EACH ROW EXECUTE FUNCTION reject_append()`,
	)
	requireTestCondition(t, err == nil, err)
	{
		_, err = appendMessage(t, f, "INBOX", original, nil)
		requireTestCondition(t, err != nil, "failed transaction acknowledged")
	}
	{
		_, err = f.store.PublishMailboxes(ctx, 100)
		requireTestCondition(t, err == nil, err)
	}
	folder, messages, err := f.store.Mailbox(ctx, f.org, f.inbox, "INBOX")
	requireTestCondition(
		t,
		((err == nil && len(messages) == 0) && folder.UIDNext == 1),
		"failed APPEND changed mailbox",
		folder,
		messages,
		err,
	)
	var count int
	{
		err = f.pool.QueryRow(ctx, `SELECT count(*) FROM fry_receipt`).Scan(&count)
		requireTestCondition(t, (err == nil && count == 0), "failed APPEND accepted a receipt", count, err)
	}
	_, err = f.pool.Exec(ctx, `DROP TRIGGER reject_append ON fry_append_result`)
	requireTestCondition(t, err == nil, err)
	result, err := appendMessage(t, f, "INBOX", original, nil)
	requireTestCondition(t, (err == nil && result.UID == 1), result, err)
}

func TestIMAPAppendRejectsMalformedWireBeforeAcceptance(t *testing.T) {
	for _, test := range []struct{ name, declared, body string }{
		{"truncated", "5", "abc"},
		{"invalid-tail", "5", "abcde invalid\r\n"},
		{"missing-tail", "5", "abcde"},
		{"empty-invalid", "0", "invalid\r\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := setup(t)
			conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", f.address)
			requireTestCondition(t, err == nil, err)
			defer func() {
				testCleanup(t)(conn.Close())
			}()
			testCheck(t)(conn.SetDeadline(time.Now().Add(5 * time.Second)))
			reader := bufio.NewReader(conn)
			{
				_, err = reader.ReadString('\n')
				requireTestCondition(t, err == nil, err)
			}
			testResult(t)(fmt.Fprint(conn, "a LOGIN test synthetic-only-password\r\n"))
			for {
				line, err := reader.ReadString('\n')
				requireTestCondition(t, err == nil, err)
				if strings.HasPrefix(line, "a OK") {
					break
				}
			}
			testResult(t)(fmt.Fprintf(conn, "b APPEND INBOX {%s}\r\n", test.declared))
			line, err := reader.ReadString('\n')
			requireTestCondition(t, (err == nil && strings.HasPrefix(line, "+")), line, err)
			testResult(t)(fmt.Fprint(conn, test.body))
			tcp, ok := conn.(*net.TCPConn)
			requireTestConditionf(t, ok, "unexpected connection: %T", conn)
			testCheck(t)(tcp.CloseWrite())
			replies, readErr := io.ReadAll(reader)
			requireTestCondition(t, (readErr == nil || errors.Is(readErr, net.ErrClosed)), readErr)
			requireTestCondition(t, !strings.Contains(string(replies), "b OK"), "malformed APPEND acknowledged")
			// Shutdown waits for started session/storage work before inspecting persistence.
			testCleanup(t)(f.server.Close())
			var count int
			{
				err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM fry_receipt`).Scan(&count)
				requireTestCondition(t, (err == nil && count == 0), "malformed APPEND persisted", count, err)
			}
		})
	}
}
