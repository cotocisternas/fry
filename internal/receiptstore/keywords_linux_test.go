//go:build linux

package receiptstore

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestMailboxKeywordBudgetsRollbackWholeMutation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	env := f.envelope(t)
	inbox := env.Destinations[0].Inbox
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	for range 2 {
		accept(t, f.s, newID(t), env, "mail")
	}
	if _, err := f.s.PublishMailboxes(ctx, 20); err != nil {
		t.Fatal(err)
	}
	first, second := []string{}, []string{}
	for i := range 128 {
		first = append(first, fmt.Sprintf("a%03d", i))
		second = append(second, fmt.Sprintf("b%03d", i))
	}
	requireReceiptNoError(t, f.s.ChangeFlags(ctx, f.org, inbox, "INBOX", []uint32{1}, "set", first))
	if err := f.s.ChangeFlags(ctx, f.org, inbox, "INBOX", []uint32{1, 2}, "add", []string{"overflow"}); err == nil {
		t.Fatal("message flag budget ignored")
	}
	folder, mail, err := f.s.Mailbox(ctx, f.org, inbox, "INBOX")
	requireReceiptCondition(t, err == nil, "partial update", folder, mail, err)
	requireReceiptCondition(t, reflect.DeepEqual(mail[0].Flags, first), "partial update", folder, mail, err)
	requireReceiptCondition(t, len(mail[1].Flags) == 0, "partial update", folder, mail, err)
	requireReceiptCondition(t, len(folder.Keywords) == 128, "partial update", folder, mail, err)
	requireReceiptNoError(t, f.s.ChangeFlags(ctx, f.org, inbox, "INBOX", []uint32{1}, "set", second))
	if err := f.s.ChangeFlags(ctx, f.org, inbox, "INBOX", []uint32{2}, "add", []string{"overflow"}); err == nil {
		t.Fatal("folder keyword budget ignored")
	}
	folder, mail, err = f.s.Mailbox(ctx, f.org, inbox, "INBOX")
	requireReceiptCondition(t, err == nil, "catalog rollback", folder, mail, err)
	requireReceiptCondition(t, reflect.DeepEqual(mail[0].Flags, second), "catalog rollback", folder, mail, err)
	requireReceiptCondition(t, len(mail[1].Flags) == 0, "catalog rollback", folder, mail, err)
	requireReceiptCondition(t, len(folder.Keywords) == 256, "catalog rollback", folder, mail, err)
	for _, bad := range []string{
		"",
		"two words",
		"line\r\nbreak",
		"é",
		"\\Recent",
		"\\Unknown",
		"wild*",
		"right]",
		strings.Repeat("x", 129),
	} {
		if err := f.s.ChangeFlags(ctx, f.org, inbox, "INBOX", []uint32{2}, "add", []string{bad}); err == nil {
			t.Fatalf("invalid keyword %q", bad)
		}
	}
}
