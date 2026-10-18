//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestFolderRenameSubtreeSubscriptionsAndVirtualParents(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	inbox := newID(t)
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	for _, name := range []string{"A_%/Invoices", "A_%/Invoices/Paid", "A_X/Keep"} {
		requireReceiptNoError(t, f.s.CreateFolder(ctx, f.org, inbox, name))
	}
	old, err := f.s.Folder(ctx, f.org, inbox, "A_%/Invoices")
	requireReceiptNoError(t, err)
	err = f.s.SubscribeFolder(ctx, f.org, inbox, "A_%/Invoices", true)
	requireReceiptNoError(t, err)
	err = f.s.RenameFolder(ctx, f.org, inbox, "A_%", "New/Archive")
	requireReceiptNoError(t, err)
	renamed, err := f.s.Folder(ctx, f.org, inbox, "New/Archive/Invoices")
	requireReceiptCondition(t, err == nil, renamed, err)
	requireReceiptCondition(t, renamed.ID == old.ID, renamed, err)
	requireReceiptCondition(t, renamed.UIDValidity == old.UIDValidity, renamed, err)
	requireReceiptCondition(t, !renamed.Subscribed, renamed, err)
	{
		_, err = f.s.Folder(ctx, f.org, inbox, "A_X/Keep")
		requireReceiptCondition(t, err == nil, "wildcard name matched sibling", err)
	}
	{
		_, err = f.s.Folder(ctx, f.org, inbox, "New")
		requireReceiptCondition(t, err == nil, "destination ancestor absent", err)
	}
	requireVirtualRenameSubscriptions(t, ctx, f, inbox)
	before, err := f.s.Folders(ctx, f.org, inbox)
	requireReceiptNoError(t, err)
	for _, destination := range []string{"New/Archive/Inner", "A_X", "A_X/Keep"} {
		{
			err = f.s.RenameFolder(ctx, f.org, inbox, "New/Archive", destination)
			requireReceiptCondition(t, err != nil, "invalid rename accepted", destination)
		}
	}
	after, err := f.s.Folders(ctx, f.org, inbox)
	requireReceiptCondition(t, err == nil, "failed rename changed namespace", err)
	requireReceiptCondition(t, reflect.DeepEqual(before, after), "failed rename changed namespace", err)
	{
		err = f.s.RenameFolder(ctx, f.org, inbox, "Missing", "Other")
		requireReceiptCondition(t, errors.Is(err, ErrNotFound), err)
	}
}

func TestFolderNameReuseAdvancesUIDValidityAndFencesOldSelection(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	inbox := newID(t)
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	requireReceiptNoError(t, f.s.CreateFolder(ctx, f.org, inbox, "Old"))
	selection, old, _, err := f.s.SelectMailbox(ctx, f.org, inbox, "Old")
	requireReceiptNoError(t, err)
	err = f.s.CreateFolder(ctx, f.org, inbox, "Reused")
	requireReceiptNoError(t, err)
	previous, err := f.s.Folder(ctx, f.org, inbox, "Reused")
	requireReceiptNoError(t, err)
	err = f.s.SubscribeFolder(ctx, f.org, inbox, "Reused", true)
	requireReceiptNoError(t, err)
	err = f.s.DeleteFolder(ctx, f.org, inbox, "Reused")
	requireReceiptNoError(t, err)
	subs, err := f.s.ListFolders(ctx, f.org, inbox, true)
	requireReceiptCondition(t, err == nil, "delete removed subscription", subs, err)
	requireReceiptCondition(t, len(subs) == 2, "delete removed subscription", subs, err)
	err = f.s.RenameFolder(ctx, f.org, inbox, "Old", "Reused")
	requireReceiptNoError(t, err)
	now, err := f.s.Folder(ctx, f.org, inbox, "Reused")
	requireReceiptCondition(t, err == nil, "unsafe name reuse", now, err)
	requireReceiptCondition(t, now.ID == old.ID, "unsafe name reuse", now, err)
	requireReceiptCondition(t, now.UIDValidity > previous.UIDValidity, "unsafe name reuse", now, err)
	requireReceiptCondition(t, now.Subscribed, "unsafe name reuse", now, err)
	{
		_, _, err = selection.View(ctx)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "old UID namespace remained selected", err)
	}
	{
		err = selection.ChangeFlags(ctx, []uint32{1}, "add", []string{"\\Seen"})
		requireReceiptCondition(t, errors.Is(err, ErrStale), "stale UID namespace mutated", err)
	}
	err = f.s.RenameFolder(ctx, f.org, inbox, "Reused", "Old")
	requireReceiptNoError(t, err)
	back, err := f.s.Folder(ctx, f.org, inbox, "Old")
	requireReceiptCondition(t, err == nil, "unnecessary or decreasing namespace change", back, err)
	requireReceiptCondition(
		t,
		back.UIDValidity == now.UIDValidity,
		"unnecessary or decreasing namespace change",
		back,
		err,
	)
}

func TestInboxRenameTransfersMailAtomicallyAndKeepsInboxIdentity(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	inbox := newID(t)
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	requireReceiptNoError(t, f.s.CreateFolder(ctx, f.org, inbox, "INBOX/Child"))
	source, err := f.s.Folder(ctx, f.org, inbox, "INBOX")
	requireReceiptNoError(t, err)
	env := Envelope{
		Organization:  f.org,
		MailboxAppend: true,
		Destinations:  []Destination{{Inbox: inbox, Folder: source.ID, Flags: []string{"\\Seen", "processed"}}},
	}
	first := accept(t, f.s, newID(t), env, "one")
	second := accept(t, f.s, newID(t), env, "two")
	selection, before, _, err := f.s.SelectMailbox(ctx, f.org, inbox, "INBOX")
	requireReceiptNoError(t, err)
	// Fail after creating destination memberships. Namespace, source memberships,
	// references, and historical destination metadata must all roll back.
	execSQL(
		t,
		f.pool,
		`CREATE FUNCTION fail_inbox_rename() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN `+
			`RAISE EXCEPTION 'fixture rename failure'; END $$`,
	)
	execSQL(
		t,
		f.pool,
		`CREATE TRIGGER fail_inbox_rename BEFORE DELETE ON fry_mail FOR EACH ROW EXECUTE `+
			`FUNCTION fail_inbox_rename()`,
	)
	{
		err = f.s.RenameFolder(ctx, f.org, inbox, "INBOX", "Failed/Archive")
		requireReceiptCondition(t, err != nil, "failed transaction succeeded")
	}
	{
		_, err = f.s.Folder(ctx, f.org, inbox, "Failed")
		requireReceiptCondition(t, errors.Is(err, ErrNotFound), "failed rename left ancestor", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_reference WHERE kind='mailbox' AND expires_at IS NULL`) == 2,
		"failed transfer changed ownership",
	)
	execSQL(t, f.pool, `DROP TRIGGER fail_inbox_rename ON fry_mail`)
	err = f.s.RenameFolder(ctx, f.org, inbox, "inbox", "History/Archive")
	requireReceiptNoError(t, err)
	requireInboxTransferIdentity(t, ctx, f, inbox, selection, before, first, second)
	third := accept(t, f.s, newID(t), env, "three")
	_, messages, err := selection.View(ctx)
	requireReceiptCondition(t, err == nil, "INBOX UID reused", messages, err)
	requireReceiptCondition(t, len(messages) == 1, "INBOX UID reused", messages, err)
	requireReceiptCondition(t, messages[0].UID == 3, "INBOX UID reused", messages, err)
	requireReceiptCondition(t, messages[0].Receipt == third.ID, "INBOX UID reused", messages, err)
}

func requireVirtualRenameSubscriptions(t *testing.T, ctx context.Context, f *fixture, inbox ID) {
	t.Helper()
	listed, err := f.s.ListFolders(ctx, f.org, inbox, false)
	requireReceiptNoError(t, err)
	virtual := false
	for _, entry := range listed {
		if entry.Name == "New/Archive" && !entry.Selectable {
			virtual = true
		}
	}
	requireReceiptCondition(t, virtual, "virtual rename source/destination not visible in LIST", listed)
	subscriptions, err := f.s.ListFolders(ctx, f.org, inbox, true)
	requireReceiptNoError(t, err)
	want := []ListedFolder{
		{Name: "A_%/Invoices", Subscribed: true},
		{Name: "INBOX", Subscribed: true, Selectable: true},
	}
	requireReceiptCondition(t, reflect.DeepEqual(subscriptions, want), "subscription moved with folder", subscriptions)
	err = f.s.SubscribeFolder(ctx, f.org, inbox, "A_%/Invoices", false)
	requireReceiptNoError(t, err)
	{
		err = f.s.SubscribeFolder(ctx, f.org, inbox, "A_%/Invoices", false)
		requireReceiptCondition(t, err == nil, "repeated unsubscribe failed", err)
	}
}

func requireInboxTransferIdentity(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	inbox ID,
	selection *MailboxSelection,
	before Folder,
	first, second Receipt,
) {
	t.Helper()
	current, messages, err := selection.View(ctx)
	requireReceiptCondition(t, err == nil, "INBOX identity changed", current, messages, err)
	requireReceiptCondition(t, current.ID == before.ID, "INBOX identity changed", current, messages, err)
	requireReceiptCondition(
		t,
		current.UIDValidity == before.UIDValidity,
		"INBOX identity changed",
		current,
		messages,
		err,
	)
	requireReceiptCondition(t, current.UIDNext == before.UIDNext, "INBOX identity changed", current, messages, err)
	requireReceiptCondition(t, len(messages) == 0, "INBOX identity changed", current, messages, err)
	requireReceiptCondition(t, current.Subscribed, "INBOX identity changed", current, messages, err)
	destination, messages, err := f.s.Mailbox(ctx, f.org, inbox, "History/Archive")
	requireReceiptCondition(t, err == nil, destination, messages, err)
	requireReceiptCondition(t, len(messages) == 2, destination, messages, err)
	requireReceiptCondition(t, destination.UIDValidity != before.UIDValidity, destination, messages, err)
	requireReceiptCondition(t, !destination.Subscribed, destination, messages, err)
	requireReceiptCondition(t, messages[0].Receipt == first.ID, destination, messages, err)
	requireReceiptCondition(t, messages[1].Receipt == second.ID, destination, messages, err)
	requireReceiptCondition(
		t,
		reflect.DeepEqual(messages[0].Flags, []string{"\\Seen", "processed"}),
		destination,
		messages,
		err,
	)
	{
		_, err = f.s.Folder(ctx, f.org, inbox, "INBOX/Child")
		requireReceiptCondition(t, err == nil, "INBOX child renamed", err)
	}
	requireReceiptCondition(
		t,
		scalarContext(
			ctx,
			t,
			f.pool,
			`SELECT count(*) FROM fry_reference WHERE kind='mailbox' AND expires_at IS NULL`,
		) == 2,
		"transferred ownership missing",
	)
}
