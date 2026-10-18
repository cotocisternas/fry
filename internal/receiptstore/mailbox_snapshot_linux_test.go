//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMailboxSnapshotDoesNotWaitForUncommittedFolderWriter(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	env := f.envelope(t)
	inbox := env.Destinations[0].Inbox
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	accepted := accept(t, f.s, newID(t), env, "old committed message")
	if _, err := f.s.PublishMailboxes(ctx, 10); err != nil {
		t.Fatal(err)
	}
	selection, folder, _, err := f.s.SelectMailbox(ctx, f.org, inbox, "INBOX")
	requireReceiptNoError(t, err)
	writer, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer rollback(context.Background(), writer)
	_, err = writer.Exec(ctx, `UPDATE fry_folder SET uidnext=uidnext+1 WHERE id=$1`, folder.ID.String())
	requireReceiptNoError(t, err)
	for _, name := range []string{"Mailbox", "View"} {
		t.Run(name, func(t *testing.T) {
			bounded, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
			defer cancel()
			var got Folder
			var mail []Mail
			var err error
			if name == "Mailbox" {
				got, mail, err = f.s.Mailbox(bounded, f.org, inbox, "INBOX")
			} else {
				got, mail, err = selection.View(bounded)
			}
			requireReceiptCondition(
				t,
				err == nil && got.UIDNext == folder.UIDNext && len(mail) == 1 && mail[0].Receipt == accepted.ID,
				"read blocked on folder writer or saw uncommitted state",
				got,
				mail,
				err,
			)
		})
	}
}

type (
	snapshotTraceKey      struct{}
	folderSnapshotBarrier struct {
		armed   atomic.Bool
		entered chan struct{}
		release chan struct{}
	}
)

func (b *folderSnapshotBarrier) TraceQueryStart(
	ctx context.Context,
	_ *pgx.Conn,
	data pgx.TraceQueryStartData,
) context.Context {
	return context.WithValue(ctx, snapshotTraceKey{}, strings.Contains(data.SQL, "FROM fry_folder f JOIN fry_inbox"))
}

func (b *folderSnapshotBarrier) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if folder, ok := ctx.Value(snapshotTraceKey{}).(bool); ok && folder && data.Err == nil &&
		b.armed.CompareAndSwap(true, false) {
		close(b.entered)
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	}
}

func snapshotReader(t *testing.T, f *fixture) (*Store, *folderSnapshotBarrier) {
	t.Helper()
	cfg := f.pool.Config().Copy()
	barrier := &folderSnapshotBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	cfg.ConnConfig.Tracer = barrier
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	requireReceiptNoError(t, err)
	t.Cleanup(pool.Close)
	return &Store{db: &authority{pool: pool, opts: f.s.db.opts}, members: f.s.members}, barrier
}

type mailboxSnapshotResult struct {
	folder Folder
	mail   []Mail
	err    error
}

func TestMailboxSnapshotIsCoherentAcrossConcurrentMutations(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	env := f.envelope(t)
	inbox := env.Destinations[0].Inbox
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	first := accept(t, f.s, newID(t), env, "first")
	second := accept(t, f.s, newID(t), env, "second")
	if _, err := f.s.PublishMailboxes(ctx, 10); err != nil {
		t.Fatal(err)
	}
	requireReceiptNoError(t, f.s.ChangeFlags(ctx, f.org, inbox, "INBOX", []uint32{1}, "add", []string{"\\Deleted"}))
	reader, barrier := snapshotReader(t, f)
	selection, before, _, err := reader.SelectMailbox(ctx, f.org, inbox, "INBOX")
	requireReceiptNoError(t, err)
	barrier.armed.Store(true)
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	done := make(chan mailboxSnapshotResult, 1)
	go func() { folder, mail, err := selection.View(bounded); done <- mailboxSnapshotResult{folder, mail, err} }()
	select {
	case <-barrier.entered:
	case <-bounded.Done():
		t.Fatal("reader did not reach folder-read barrier")
	}
	release := sync.OnceFunc(func() { close(barrier.release) })
	defer release()
	if err = f.s.ChangeFlags(bounded, f.org, inbox, "INBOX", []uint32{2}, "add", []string{"\\Seen"}); err != nil {
		t.Fatal("read snapshot blocked flags", err)
	}
	if _, err = f.s.Expunge(bounded, f.org, inbox, "INBOX", []uint32{1}); err != nil {
		t.Fatal("read snapshot blocked expunge", err)
	}
	third := accept(t, f.s, newID(t), env, "third")
	if n, err := f.s.PublishMailboxes(bounded, 10); err != nil || n != 1 {
		t.Fatal("read snapshot blocked UID publication", n, err)
	}
	release()
	requireSnapshotBeforeMutation(t, <-done, before, first, second)
	requireSnapshotAfterMutation(t, ctx, selection, before, second, third)
}

func TestMailboxSnapshotCancellationReleasesTransaction(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	inbox := newID(t)
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	reader, barrier := snapshotReader(t, f)
	barrier.armed.Store(true)
	canceled, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, _, err := reader.Mailbox(canceled, f.org, inbox, "INBOX"); done <- err }()
	select {
	case <-barrier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not reach barrier")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("canceled snapshot succeeded", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for reader.db.pool.Stat().AcquiredConns() != 0 {
		requireReceiptCondition(t, !time.Now().After(deadline), "canceled read retained transaction/connection")
		time.Sleep(time.Millisecond)
	}
	// The canceled operation cannot leave a snapshot or row lock holding writers.
	bounded, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	requireReceiptNoError(t, f.s.CreateFolder(bounded, f.org, inbox, "after cancellation"))
}

func TestMailboxSnapshotScopeAndStaging(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	inbox := newID(t)
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	if _, _, err := f.s.Mailbox(ctx, newID(t), inbox, "INBOX"); !errors.Is(err, ErrNotFound) {
		t.Fatal("snapshot leaked organization", err)
	}
	job, staging := newID(t), newID(t)
	requireReceiptNoError(
		t,
		f.s.CreateMigration(ctx, f.org, job, staging, FingerprintMigrationSource("source:993", "user", false)),
	)
	if _, _, err := f.s.Mailbox(ctx, f.org, staging, "INBOX"); !errors.Is(err, ErrNotFound) {
		t.Fatal("snapshot exposed staging", err)
	}
}

func TestMailboxSnapshotKeepsIdentityAcrossConcurrentNamespaceReplacement(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	inbox := newID(t)
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	requireReceiptNoError(t, f.s.CreateFolder(ctx, f.org, inbox, "Selected"))
	folder, err := f.s.Folder(ctx, f.org, inbox, "Selected")
	requireReceiptNoError(t, err)
	original := accept(
		t,
		f.s,
		newID(t),
		Envelope{
			Organization:  f.org,
			MailboxAppend: true,
			Destinations:  []Destination{{Inbox: inbox, Folder: folder.ID}},
		},
		"old identity",
	)
	reader, barrier := snapshotReader(t, f)
	selection, _, _, err := reader.SelectMailbox(ctx, f.org, inbox, "Selected")
	requireReceiptNoError(t, err)
	barrier.armed.Store(true)
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	done := make(chan mailboxSnapshotResult, 1)
	go func() { folder, mail, err := selection.View(bounded); done <- mailboxSnapshotResult{folder, mail, err} }()
	select {
	case <-barrier.entered:
	case <-bounded.Done():
		t.Fatal("folder barrier not reached")
	}
	release := sync.OnceFunc(func() { close(barrier.release) })
	defer release()
	err = f.s.RenameFolder(bounded, f.org, inbox, "Selected", "Renamed")
	requireReceiptNoError(t, err)
	err = f.s.CreateFolder(bounded, f.org, inbox, "Selected")
	requireReceiptNoError(t, err)
	err = f.s.DeleteFolder(bounded, f.org, inbox, "Renamed")
	requireReceiptNoError(t, err)
	err = f.s.CreateFolder(bounded, f.org, inbox, "Renamed")
	requireReceiptNoError(t, err)
	release()
	requireOriginalNamespaceSnapshot(t, <-done, folder, original)
	if _, _, err = selection.View(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatal("next View followed reused name", err)
	}
	requireReplacementNamespaceEmpty(t, ctx, f, inbox, folder)
}

func requireSnapshotBeforeMutation(t *testing.T, old mailboxSnapshotResult, before Folder, first, second Receipt) {
	t.Helper()
	requireReceiptCondition(t, old.err == nil, "mixed folder/message snapshots", old)
	requireReceiptCondition(t, old.folder.UIDNext == before.UIDNext, "mixed folder/message snapshots", old)
	requireReceiptCondition(t, len(old.mail) == 2, "mixed folder/message snapshots", old)
	requireReceiptCondition(t, old.mail[0].Receipt == first.ID, "mixed folder/message snapshots", old)
	requireReceiptCondition(t, old.mail[1].Receipt == second.ID, "mixed folder/message snapshots", old)
	requireReceiptCondition(
		t,
		reflect.DeepEqual(old.mail[0].Flags, []string{"\\Deleted"}),
		"mixed folder/message snapshots",
		old,
	)
	requireReceiptCondition(t, len(old.mail[1].Flags) == 0, "mixed folder/message snapshots", old)
}

func requireSnapshotAfterMutation(
	t *testing.T,
	ctx context.Context,
	selection *MailboxSelection,
	before Folder,
	second, third Receipt,
) {
	t.Helper()
	after, mail, err := selection.View(ctx)
	requireReceiptCondition(t, err == nil, "next snapshot missed committed mutations", after, mail, err)
	requireReceiptCondition(
		t,
		after.UIDNext == before.UIDNext+1,
		"next snapshot missed committed mutations",
		after,
		mail,
		err,
	)
	requireReceiptCondition(t, len(mail) == 2, "next snapshot missed committed mutations", after, mail, err)
	requireReceiptCondition(t, mail[0].UID == 2, "next snapshot missed committed mutations", after, mail, err)
	requireReceiptCondition(
		t,
		mail[0].Receipt == second.ID,
		"next snapshot missed committed mutations",
		after,
		mail,
		err,
	)
	requireReceiptCondition(
		t,
		reflect.DeepEqual(mail[0].Flags, []string{"\\Seen"}),
		"next snapshot missed committed mutations",
		after,
		mail,
		err,
	)
	requireReceiptCondition(t, mail[1].UID == 3, "next snapshot missed committed mutations", after, mail, err)
	requireReceiptCondition(
		t,
		mail[1].Receipt == third.ID,
		"next snapshot missed committed mutations",
		after,
		mail,
		err,
	)
}

func requireOriginalNamespaceSnapshot(t *testing.T, old mailboxSnapshotResult, folder Folder, original Receipt) {
	t.Helper()
	requireReceiptCondition(t, old.err == nil, "namespace replacement mixed snapshot identity", old)
	requireReceiptCondition(t, old.folder.ID == folder.ID, "namespace replacement mixed snapshot identity", old)
	requireReceiptCondition(t, old.folder.Name == "Selected", "namespace replacement mixed snapshot identity", old)
	requireReceiptCondition(
		t,
		old.folder.UIDValidity == folder.UIDValidity,
		"namespace replacement mixed snapshot identity",
		old,
	)
	requireReceiptCondition(t, len(old.mail) == 1, "namespace replacement mixed snapshot identity", old)
	requireReceiptCondition(t, old.mail[0].Receipt == original.ID, "namespace replacement mixed snapshot identity", old)
}

func requireReplacementNamespaceEmpty(t *testing.T, ctx context.Context, f *fixture, inbox ID, folder Folder) {
	t.Helper()
	replacement, mail, err := f.s.Mailbox(ctx, f.org, inbox, "Selected")
	requireReceiptCondition(t, err == nil, "replacement inherited old selection", replacement, mail, err)
	requireReceiptCondition(
		t,
		replacement.ID != folder.ID,
		"replacement inherited old selection",
		replacement,
		mail,
		err,
	)
	requireReceiptCondition(
		t,
		replacement.UIDValidity != folder.UIDValidity,
		"replacement inherited old selection",
		replacement,
		mail,
		err,
	)
	requireReceiptCondition(t, len(mail) == 0, "replacement inherited old selection", replacement, mail, err)
}
