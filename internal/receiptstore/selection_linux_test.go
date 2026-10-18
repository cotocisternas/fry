//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"testing"
)

func TestMailboxSelectionSurvivesRenameAndNeverFollowsReusedName(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	inbox := newID(t)
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	requireReceiptNoError(t, f.s.CreateFolder(ctx, f.org, inbox, "Selected"))
	requireReceiptNoError(t, f.s.CreateFolder(ctx, f.org, inbox, "Copies"))
	folder, err := f.s.Folder(ctx, f.org, inbox, "Selected")
	requireReceiptNoError(t, err)
	env := Envelope{
		Organization:  f.org,
		MailboxAppend: true,
		Destinations:  []Destination{{Inbox: inbox, Folder: folder.ID}},
	}
	original := accept(t, f.s, newID(t), env, "selected original")
	selection, first, _, err := f.s.SelectMailbox(ctx, f.org, inbox, "Selected")
	requireReceiptNoError(t, err)
	err = f.s.RenameFolder(ctx, f.org, inbox, "Selected", "Renamed")
	requireReceiptNoError(t, err)
	err = f.s.CreateFolder(ctx, f.org, inbox, "Selected")
	requireReceiptNoError(t, err)
	replacement, err := f.s.Folder(ctx, f.org, inbox, "Selected")
	requireReceiptNoError(t, err)
	env.Destinations[0].Folder = replacement.ID
	replacementMail := accept(t, f.s, newID(t), env, "replacement must be untouched")
	err = selection.ChangeFlags(ctx, []uint32{1}, "add", []string{"\\Deleted"})
	requireReceiptNoError(t, err)
	_, err = selection.CopyTo(ctx, "Copies", []uint32{1}, false)
	requireReceiptNoError(t, err)
	_, err = selection.Expunge(ctx, []uint32{1})
	requireReceiptNoError(t, err)
	requireSelectedMailboxViews(t, ctx, f, inbox, selection, first, replacementMail, original)
	err = f.s.DeleteFolder(ctx, f.org, inbox, "Renamed")
	requireReceiptNoError(t, err)
	err = f.s.CreateFolder(ctx, f.org, inbox, "Renamed")
	requireReceiptNoError(t, err)
	if _, _, err = selection.View(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatal("selection followed replacement", err)
	}
	if err = selection.ChangeFlags(ctx, []uint32{1}, "add", []string{"\\Seen"}); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted identity permitted mutation", err)
	}
}

func requireSelectedMailboxViews(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	inbox ID,
	selection *MailboxSelection,
	first Folder,
	replacementMail, original Receipt,
) {
	t.Helper()
	current, messages, err := selection.View(ctx)
	requireReceiptCondition(t, err == nil, current, messages, err)
	requireReceiptCondition(t, current.ID == first.ID, current, messages, err)
	requireReceiptCondition(t, current.Name == "Renamed", current, messages, err)
	requireReceiptCondition(t, len(messages) == 0, current, messages, err)
	_, messages, err = f.s.Mailbox(ctx, f.org, inbox, "Selected")
	requireReceiptCondition(t, err == nil, "replacement was mutated", messages, err)
	requireReceiptCondition(t, len(messages) == 1, "replacement was mutated", messages, err)
	requireReceiptCondition(t, messages[0].Receipt == replacementMail.ID, "replacement was mutated", messages, err)
	requireReceiptCondition(t, len(messages[0].Flags) == 0, "replacement was mutated", messages, err)
	_, messages, err = f.s.Mailbox(ctx, f.org, inbox, "Copies")
	requireReceiptCondition(t, err == nil, "wrong copy source", messages, err)
	requireReceiptCondition(t, len(messages) == 1, "wrong copy source", messages, err)
	requireReceiptCondition(t, messages[0].Receipt == original.ID, "wrong copy source", messages, err)
}
