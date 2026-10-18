//go:build linux

package imapserver

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/emersion/go-imap/v2"
)

func TestIMAPSelectedFolderIdentitySurvivesAnotherClientsRename(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	{
		err := f.store.CreateFolder(ctx, f.org, f.inbox, "Work")
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err := appendMessage(t, f, "Work", original, nil)
		requireTestCondition(t, err == nil, err)
	}
	a, b := f.client(t), f.client(t)
	before, err := a.Select("Work", nil).Wait()
	requireTestCondition(t, err == nil, err)
	{
		err = b.Rename("Work", "Archive", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		err = b.Create("Work", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	const replacement = "Subject: replacement\r\n\r\nreplacement\r\n"
	{
		_, err = appendMessage(t, f, "Work", replacement, nil)
		requireTestCondition(t, err == nil, err)
	}
	requireRenamedSelectionKeepsIdentity(t, f, ctx, a, before)
	requireDeletedSelectionDoesNotMutateReplacement(t, f, ctx, a, b, replacement)
}

func TestIMAPSameFolderCopyMoveAndEmptyUIDSets(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for range 3 {
		{
			_, err := appendMessage(t, f, "INBOX", original, nil)
			requireTestCondition(t, err == nil, err)
		}
	}
	{
		err := f.store.ChangeFlags(ctx, f.org, f.inbox, "INBOX", []uint32{3}, "add", []string{"\\Deleted"})
		requireTestCondition(t, err == nil, err)
	}
	c := f.client(t)
	{
		_, err := c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
		requireTestCondition(t, err == nil, err)
	}
	// EXAMINE protects selected-source mutations; it does not change destination
	// permissions for an independently named COPY/APPEND target.
	copied, err := c.Copy(imap.UIDSetNum(1), "INBOX").Wait()
	requireTestCondition(t, ((err == nil && copied != nil) && copied.DestUIDs.Contains(4)), copied, err)
	{
		_, err = c.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	moved, err := c.Move(imap.UIDSetNum(2), "INBOX").Wait()
	requireTestCondition(t, ((err == nil && moved != nil) && moved.DestUIDs.String() == "5"), moved, err)
	_, messages, err := f.store.Mailbox(ctx, f.org, f.inbox, "INBOX")
	requireTestCondition(t, (err == nil && len(messages) == 4), messages, err)
	expected := []uint32{1, 3, 4, 5}
	for i, m := range messages {
		requireTestCondition(t, m.UID == expected[i], "move did not reallocate UID", messages)
	}
	requireTestCondition(t, hasFlag(messages[1], "\\Deleted"), "move expunged unrelated deleted message")
	{
		_, err = c.Copy(imap.UIDSetNum(999), "INBOX").Wait()
		requireTestCondition(t, err == nil, "empty UID COPY response malformed", err)
	}
	{
		_, err = c.Move(imap.UIDSetNum(999), "INBOX").Wait()
		requireTestCondition(t, err == nil, "empty UID MOVE response malformed", err)
	}
	selected, err := c.Select("INBOX", nil).Wait()
	requireTestCondition(
		t,
		((err == nil && selected.UIDNext == 6) && selected.NumMessages == 4),
		"empty operation consumed UID or changed contents",
		selected,
		err,
	)
}

func TestIMAPInboxRenameKeepsSelectedInboxAndSubscriptionNames(t *testing.T) {
	f := setup(t)
	a, b := f.client(t), f.client(t)
	{
		err := a.Create("INBOX/Child", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err := appendMessage(t, f, "INBOX", original, nil)
		requireTestCondition(t, err == nil, err)
	}
	before, err := a.Select("INBOX", nil).Wait()
	requireTestCondition(t, err == nil, err)
	{
		err = b.Rename("INBOX", "Old/Inbox", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		err = a.Noop().Wait()
		requireTestCondition(t, err == nil, err)
	}
	result, err := appendMessage(t, f, "INBOX", original, nil)
	requireTestCondition(t, (err == nil && result.UID == 2), result, err)
	{
		err = a.Noop().Wait()
		requireTestCondition(t, err == nil, err)
	}
	got, err := a.Fetch(imap.UIDSetNum(2), &imap.FetchOptions{UID: true}).Collect()
	requireTestCondition(
		t,
		((err == nil && len(got) == 1) && got[0].UID == 2),
		"session followed renamed content",
		got,
		err,
	)
	after, err := a.Select("INBOX", nil).Wait()
	requireTestCondition(
		t,
		((err == nil && after.UIDValidity == before.UIDValidity) && after.NumMessages == 1),
		after,
		err,
	)
	requireRenamedSubscriptionKeepsOriginalName(t, b)
}

func TestIMAPListSeparatorVirtualParentsAndLegacySubscriptions(t *testing.T) {
	f := setup(t)
	c := f.client(t)
	{
		err := c.Create("Parent/Child", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		err := c.Subscribe("Parent/Child").Wait()
		requireTestCondition(t, err == nil, err)
	}
	root, err := c.List("", "", nil).Collect()
	requireTestCondition(
		t,
		(((err == nil && len(root) == 1) && root[0].Delim == '/') && root[0].Mailbox == ""),
		"missing namespace separator",
		root,
		err,
	)
	listed, err := c.List("", "%", nil).Collect()
	requireTestCondition(t, err == nil, err)
	parent := hasVirtualParent(listed, "Parent")

	requireTestCondition(t, parent, "virtual parent absent", listed)
	command := subscriptionWire(t, f.address)
	result := command("b", `LSUB "" "%"`)
	requireTestCondition(
		t,
		(strings.Contains(result, `* LSUB (\Noselect) "/" "Parent"`) && !strings.Contains(result, `"Parent/Child"`)),
		"LSUB percent parent response",
		result,
	)
	{
		err = c.Delete("Parent/Child").Wait()
		requireTestCondition(t, err == nil, err)
	}
	result = command("c", `LSUB "" "*"`)
	requireTestCondition(t, strings.Contains(result, `"Parent/Child"`), "deleted subscription disappeared", result)
	command("d", `UNSUBSCRIBE "Parent/Child"`)
	result = command("e", `LSUB "" "*"`)
	requireTestCondition(t, !strings.Contains(result, `"Parent/Child"`), "unsubscribe did not remove name", result)
}

func hasVirtualParent(listed []*imap.ListData, name string) bool {
	for _, entry := range listed {
		if entry.Mailbox != name {
			continue
		}
		for _, attr := range entry.Attrs {
			if attr == imap.MailboxAttrNoSelect {
				return true
			}
		}
	}
	return false
}

func subscriptionWire(t *testing.T, address string) func(string, string) string {
	t.Helper()
	// Exercise actual LSUB, which the pinned high-level client doesn't expose.
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
	requireTestCondition(t, err == nil, err)
	t.Cleanup(func() { testCleanup(t)(conn.Close()) })
	testCheck(t)(conn.SetDeadline(time.Now().Add(10 * time.Second)))
	reader := bufio.NewReader(conn)
	{
		_, err = reader.ReadString('\n')
		requireTestCondition(t, err == nil, err)
	}
	command := func(tag, cmd string) string {
		t.Helper()
		{
			_, e := fmt.Fprintf(conn, "%s %s\r\n", tag, cmd)
			requireTestCondition(t, e == nil, e)
		}
		var result strings.Builder
		for {
			line, e := reader.ReadString('\n')
			requireTestCondition(t, e == nil, e)
			testResult(t)(result.WriteString(line))
			if strings.HasPrefix(line, tag+" ") {
				requireTestCondition(t, strings.HasPrefix(line, tag+" OK "), result.String())
				return result.String()
			}
		}
	}
	command("a", `LOGIN test synthetic-only-password`)
	return command
}

func requireDeletedSelectionDoesNotMutateReplacement(
	t *testing.T,
	f *fixture,
	ctx context.Context,
	a, b *imapclient.Client,
	replacement string,
) {
	t.Helper()
	var err error
	{
		err = b.Delete("Archive").Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		err = b.Create("Archive", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err = appendMessage(t, f, "Archive", replacement, nil)
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err = a.Store(
			imap.UIDSetNum(1),
			&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}},
			nil,
		).
			Collect()
		requireTestCondition(t, err != nil, "deleted selection mutated replacement")
	}
	_, mail, err := f.store.Mailbox(ctx, f.org, f.inbox, "Archive")
	requireTestCondition(
		t,
		((err == nil && len(mail) == 1) && !hasFlag(mail[0], "\\Deleted")),
		"deleted selection changed replacement",
		mail,
		err,
	)
}

func requireRenamedSelectionKeepsIdentity(
	t *testing.T,
	f *fixture,
	ctx context.Context,
	a *imapclient.Client,
	before *imap.SelectData,
) {
	t.Helper()
	section := &imap.FetchItemBodySection{Peek: true}
	fetched, err := a.Fetch(
		imap.UIDSetNum(1),
		&imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}},
	).
		Collect()
	requireTestCondition(
		t,
		((err == nil && len(fetched) == 1) && string(fetched[0].FindBodySection(section)) == original),
		"selected identity changed",
		fetched,
		err,
	)
	{
		_, err = a.Store(
			imap.UIDSetNum(1),
			&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}},
			nil,
		).
			Collect()
		requireTestCondition(t, err == nil, err)
	}
	archived, mail, err := f.store.Mailbox(ctx, f.org, f.inbox, "Archive")
	requireTestCondition(
		t,
		(((err == nil && archived.UIDValidity == before.UIDValidity) &&
			len(mail) == 1) &&
			hasFlag(mail[0], "\\Flagged")),
		archived,
		mail,
		err,
	)
	_, mail, err = f.store.Mailbox(ctx, f.org, f.inbox, "Work")
	requireTestCondition(
		t,
		((err == nil && len(mail) == 1) && !hasFlag(mail[0], "\\Flagged")),
		"replacement modified",
		mail,
		err,
	)
}

func requireRenamedSubscriptionKeepsOriginalName(t *testing.T, b *imapclient.Client) {
	t.Helper()
	var err error
	{
		err = b.Subscribe("Old/Inbox").Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		err = b.Rename("Old", "History", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	listed, err := b.List("", "*", &imap.ListOptions{SelectSubscribed: true}).Collect()
	requireTestCondition(t, err == nil, err)
	retained := false
	for _, entry := range listed {
		if entry.Mailbox == "Old/Inbox" {
			retained = true
		}
	}
	requireTestCondition(t, retained, "rename removed original-name subscription", listed)
	{
		err = b.Unsubscribe("Old/Inbox").Wait()
		requireTestCondition(t, err == nil, err)
	}
}
