//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestMailboxPublicationAndLifetime(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	env := f.envelope(t)
	inbox := env.Destinations[0].Inbox
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	{
		err := f.s.EnsureInbox(ctx, newID(t), inbox)
		requireReceiptCondition(t, errors.Is(err, ErrConflict), "inbox tenant reassignment", err)
	}
	r := accept(t, f.s, newID(t), env, "original")
	publishMailboxConcurrently(t, ctx, f)
	folder, messages, err := f.s.Mailbox(ctx, f.org, inbox, "inbox")
	requireReceiptCondition(t, err == nil, folder, messages, err)
	requireReceiptCondition(t, len(messages) == 1, folder, messages, err)
	requireReceiptCondition(t, messages[0].Receipt == r.ID, folder, messages, err)
	requireReceiptCondition(t, messages[0].UID == 1, folder, messages, err)
	requireReceiptCondition(t, folder.UIDNext == 2, folder, messages, err)
	requireReceiptCondition(t, scalar(t, f.pool, `SELECT count(*) FROM fry_work`) == 0, "pending work remains")
	execSQL(t, f.pool, `UPDATE fry_reference SET expires_at=clock_timestamp()-interval '1 day' WHERE `+
		`kind='recovery'`)
	{
		n, err := f.s.SweepExpiry(ctx, 10)
		requireReceiptCondition(t, n == 0, "visible mail expired", n, err)
		requireReceiptCondition(t, err == nil, "visible mail expired", n, err)
	}
	err = f.s.ChangeFlags(ctx, f.org, inbox, "INBOX", []uint32{1}, "add", []string{"\\Seen", "\\Deleted"})
	requireReceiptNoError(t, err)
	removed, err := f.s.Expunge(ctx, f.org, inbox, "INBOX", []uint32{1})
	requireReceiptCondition(t, err == nil, removed, err)
	requireReceiptCondition(t, reflect.DeepEqual(removed, []uint32{1}), removed, err)
	status, err := f.s.CleanupStatus(ctx, f.org, r.ID)
	requireReceiptCondition(t, err == nil, status, err)
	requireReceiptCondition(t, status.State == "retiring", status, err)
	{
		n, err := f.s.PublishMailboxes(ctx, 10)
		requireReceiptCondition(t, n == 0, "worker resurrected expunged membership", n, err)
		requireReceiptCondition(t, err == nil, "worker resurrected expunged membership", n, err)
	}
	requireMailboxUIDContinuity(t, f, inbox, env, folder)
}

func TestMailboxFlagsAndSelectiveExpunge(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	env := f.envelope(t)
	inbox := env.Destinations[0].Inbox
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	for range 3 {
		accept(t, f.s, newID(t), env, "message")
	}
	{
		_, err := f.s.PublishMailboxes(ctx, 20)
		requireReceiptCondition(t, err == nil, err)
	}
	requireReceiptNoError(
		t,
		f.s.ChangeFlags(ctx, f.org, inbox, "INBOX", []uint32{1, 2}, "set", []string{"\\deleted", "\\SEEN", "\\Seen"}),
	)
	requireReceiptNoError(t, f.s.ChangeFlags(ctx, f.org, inbox, "INBOX", []uint32{2}, "remove", []string{"\\Seen"}))
	{
		err := f.s.ChangeFlags(ctx, f.org, inbox, "INBOX", []uint32{3}, "add", []string{"\\Recent"})
		requireReceiptCondition(t, err != nil, "recent persisted")
	}
	removed, err := f.s.Expunge(ctx, f.org, inbox, "INBOX", []uint32{2, 3})
	requireReceiptCondition(t, err == nil, removed, err)
	requireReceiptCondition(t, reflect.DeepEqual(removed, []uint32{2}), removed, err)
	_, mail, err := f.s.Mailbox(ctx, f.org, inbox, "INBOX")
	requireReceiptCondition(t, err == nil, mail, err)
	requireReceiptCondition(t, len(mail) == 2, mail, err)
	requireReceiptCondition(t, mail[0].UID == 1, mail, err)
	requireReceiptCondition(t, mail[1].UID == 3, mail, err)
	requireReceiptCondition(t, reflect.DeepEqual(mail[0].Flags, []string{"\\Deleted", "\\Seen"}), mail, err)
	err = f.s.CreateFolder(ctx, f.org, inbox, "Archive")
	requireReceiptNoError(t, err)
	err = f.s.SubscribeFolder(ctx, f.org, inbox, "Archive", true)
	requireReceiptNoError(t, err)
	folders, err := f.s.Folders(ctx, f.org, inbox)
	requireReceiptCondition(t, err == nil, folders, err)
	requireReceiptCondition(t, len(folders) == 2, folders, err)
	requireReceiptCondition(t, folders[0].Subscribed, folders, err)
}

func TestMailboxCopyMoveDeletePreserveOwnership(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	env := f.envelope(t)
	inbox := env.Destinations[0].Inbox
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	r := accept(t, f.s, newID(t), env, "must survive")
	{
		_, err := f.s.PublishMailboxes(ctx, 10)
		requireReceiptCondition(t, err == nil, err)
	}
	execSQL(t, f.pool, `UPDATE fry_reference SET expires_at=clock_timestamp()-interval '1 day' WHERE `+
		`kind='recovery'`)
	for _, name := range []string{"Archive", "Processed"} {
		requireReceiptNoError(t, f.s.CreateFolder(ctx, f.org, inbox, name))
	}
	copied, err := f.s.CopyMail(ctx, f.org, inbox, "INBOX", "Archive", []uint32{1}, false)
	requireReceiptCondition(t, err == nil, copied, err)
	requireReceiptCondition(t, reflect.DeepEqual(copied.Source, []uint32{1}), copied, err)
	requireReceiptCondition(t, reflect.DeepEqual(copied.Destination, []uint32{1}), copied, err)
	moved, err := f.s.CopyMail(ctx, f.org, inbox, "INBOX", "Processed", []uint32{1}, true)
	requireReceiptCondition(t, err == nil, moved, err)
	requireReceiptCondition(t, len(moved.Source) == 1, moved, err)
	_, mail, err := f.s.Mailbox(ctx, f.org, inbox, "INBOX")
	requireReceiptCondition(t, err == nil, mail, err)
	requireReceiptCondition(t, len(mail) == 0, mail, err)
	err = f.s.DeleteFolder(ctx, f.org, inbox, "Archive")
	requireReceiptNoError(t, err)
	status, err := f.s.CleanupStatus(ctx, f.org, r.ID)
	requireReceiptCondition(t, err == nil, "another folder's copy lost", status, err)
	requireReceiptCondition(t, status.State == "live", "another folder's copy lost", status, err)
	requireMovedMailboxRenameLifecycle(t, ctx, f, inbox, r)
}

func TestMissingDestinationDoesNotBlockOtherPublication(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	env := f.envelope(t)
	inbox := env.Destinations[0].Inbox
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	bad := env
	bad.Destinations = append([]Destination(nil), env.Destinations...)
	bad.Destinations[0].Folder = newID(t)
	accepted := accept(t, f.s, newID(t), bad, "pending missing destination")
	accept(t, f.s, newID(t), env, "healthy destination")
	n, err := f.s.PublishMailboxes(ctx, 100)
	requireReceiptCondition(t, n == 1, n, err)
	requireReceiptCondition(t, err != nil, n, err)
	_, mail, err := f.s.Mailbox(ctx, f.org, inbox, "INBOX")
	requireReceiptCondition(t, err == nil, mail, err)
	requireReceiptCondition(t, len(mail) == 1, mail, err)
	status, err := f.s.CleanupStatus(ctx, f.org, accepted.ID)
	requireReceiptCondition(t, err == nil, "pending original lost", status, err)
	requireReceiptCondition(t, status.State == "live", "pending original lost", status, err)
	n, err = f.s.PublishMailboxes(ctx, 100)
	requireReceiptCondition(t, n == 0, "failed work ignored retry delay", n, err)
	requireReceiptCondition(t, err == nil, "failed work ignored retry delay", n, err)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_work WHERE failures=1 AND `+
				`last_error='destination_unavailable'`,
		) == 1,
		"missing durable failure debt",
	)
}

func publishMailboxConcurrently(t *testing.T, ctx context.Context, f *fixture) {
	t.Helper()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := f.s.PublishMailboxes(ctx, 20); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func requireMailboxUIDContinuity(t *testing.T, f *fixture, inbox ID, env Envelope, folder Folder) {
	t.Helper()
	ctx := context.Background()
	var err error
	err = f.s.EnsureInbox(ctx, f.org, inbox)
	requireReceiptNoError(t, err)
	after, messages, err := f.s.Mailbox(ctx, f.org, inbox, "INBOX")
	requireReceiptCondition(t, err == nil, after, messages, err)
	requireReceiptCondition(t, len(messages) == 0, after, messages, err)
	requireReceiptCondition(t, after.UIDValidity == folder.UIDValidity, after, messages, err)
	requireReceiptCondition(t, after.UIDNext == 2, after, messages, err)
	accept(t, f.s, newID(t), env, "new original")
	_, err = f.s.PublishMailboxes(ctx, 10)
	requireReceiptNoError(t, err)
	_, messages, err = f.s.Mailbox(ctx, f.org, inbox, "INBOX")
	requireReceiptCondition(t, err == nil, "UID reused", messages, err)
	requireReceiptCondition(t, len(messages) == 1, "UID reused", messages, err)
	requireReceiptCondition(t, messages[0].UID == 2, "UID reused", messages, err)
	{
		_, _, err = f.s.Mailbox(ctx, newID(t), inbox, "INBOX")
		requireReceiptCondition(t, errors.Is(err, ErrNotFound), "tenant leak", err)
	}
}

func requireMovedMailboxRenameLifecycle(t *testing.T, ctx context.Context, f *fixture, inbox ID, r Receipt) {
	t.Helper()
	old, _, err := f.s.Mailbox(ctx, f.org, inbox, "Processed")
	requireReceiptNoError(t, err)
	err = f.s.RenameFolder(ctx, f.org, inbox, "Processed", "Done")
	requireReceiptNoError(t, err)
	renamed, _, err := f.s.Mailbox(ctx, f.org, inbox, "Done")
	requireReceiptCondition(t, err == nil, renamed, err)
	requireReceiptCondition(t, renamed.UIDValidity == old.UIDValidity, renamed, err)
	err = f.s.DeleteFolder(ctx, f.org, inbox, "Done")
	requireReceiptNoError(t, err)
	status, err := f.s.CleanupStatus(ctx, f.org, r.ID)
	requireReceiptCondition(t, err == nil, status, err)
	requireReceiptCondition(t, status.State == "retiring", status, err)
	err = f.s.CreateFolder(ctx, f.org, inbox, "Done")
	requireReceiptNoError(t, err)
	recreated, _, err := f.s.Mailbox(ctx, f.org, inbox, "Done")
	requireReceiptCondition(t, err == nil, "recreated folder reused UIDVALIDITY", recreated, err)
	requireReceiptCondition(
		t,
		recreated.UIDValidity != old.UIDValidity,
		"recreated folder reused UIDVALIDITY",
		recreated,
		err,
	)
}
