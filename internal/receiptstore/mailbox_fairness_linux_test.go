//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func fairnessID(n byte) ID { var id ID; id[15] = n; return id }
func fairnessQueue(t *testing.T, f *fixture, org, inbox ID, n int) []Receipt {
	t.Helper()
	ctx := context.Background()
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, org, inbox))
	var receipts []Receipt
	for range n {
		env := Envelope{
			Organization: org,
			MailFrom:     "sender@example.test",
			Destinations: []Destination{{Inbox: inbox, Recipients: []string{"recipient@example.test"}}},
		}
		receipts = append(receipts, accept(t, f.s, newID(t), env, "synthetic fairness original"))
	}
	return receipts
}

func TestMailboxOrganizationTurnsRotateBeforeBacklogDrains(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	orgs := []ID{fairnessID(1), fairnessID(2), fairnessID(3)}
	inboxes := []ID{fairnessID(11), fairnessID(12), fairnessID(13)}
	receipts := [][]Receipt{
		fairnessQueue(t, f, orgs[0], inboxes[0], 7),
		fairnessQueue(t, f, orgs[1], inboxes[1], 2),
		fairnessQueue(t, f, orgs[2], inboxes[2], 2),
	}
	{
		n := scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_work w JOIN fry_receipt r ON r.id=w.receipt WHERE `+
				`w.organization<>r.organization OR w.received_at<>r.received_at`,
		)
		requireReceiptCondition(t, n == 0, "scheduling metadata differs from immutable receipt", n)
	}
	want := []int{0, 1, 2, 0}
	counts := []int{0, 0, 0}
	for turn, index := range want {
		n, err := f.s.PublishMailboxes(ctx, 2)
		requireReceiptCondition(t, err == nil, turn, n, err)
		requireReceiptCondition(t, n == 2, turn, n, err)
		counts[index] += 2
		for j := range orgs {
			folder, messages, err := f.s.Mailbox(ctx, orgs[j], inboxes[j], "INBOX")
			requireReceiptConditionf(
				t,
				err == nil,
				"turn%d org%d want%d got%d: %v",
				turn,
				j,
				counts[j],
				len(messages),
				err,
			)
			requireReceiptConditionf(
				t,
				len(messages) == counts[j],
				"turn%d org%d want%d got%d: %v",
				turn,
				j,
				counts[j],
				len(messages),
				err,
			)
			count := counts[j]
			if count < 0 || count > 1024 {
				t.Fatal("invalid publication count")
				return
			}
			requireReceiptCondition(t, folder.UIDNext == uint64(count)+1, folder)
			for k, m := range messages {
				requireReceiptCondition(t, m.Receipt == receipts[j][k].ID, "within-organization order", j, k, m)
				requireReceiptCondition(t, m.UID == uint32(k+1), "within-organization order", j, k, m)
			}
		}
	}
}

func TestMailboxOrganizationPoisonDoesNotRetainTurn(t *testing.T) {
	for _, released := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleted folder", true: "released reference"}[released], func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			a, b := fairnessID(1), fairnessID(2)
			ia, ib := fairnessID(11), fairnessID(12)
			requireReceiptNoError(t, f.s.EnsureInbox(ctx, a, ia))
			env := Envelope{
				Organization: a,
				MailFrom:     "sender@example.test",
				Destinations: []Destination{{Inbox: ia, Recipients: []string{"recipient@example.test"}}},
			}
			if !released {
				requireReceiptNoError(t, f.s.CreateFolder(ctx, a, ia, "Removed"))
				folder, _, err := f.s.Mailbox(ctx, a, ia, "Removed")
				requireReceiptNoError(t, err)
				env.Destinations[0].Folder = folder.ID
			}
			receipt := accept(t, f.s, newID(t), env, "poison stays durable")
			if released {
				requireReceiptNoError(
					t,
					f.s.ReleaseReference(ctx, a, receipt.ID, Reference{Kind: "pending-mailbox", Key: ia.String()}),
				)
			} else {
				requireReceiptNoError(t, f.s.DeleteFolder(ctx, a, ia, "Removed"))
			}
			fairnessQueue(t, f, b, ib, 1)
			{
				n, err := f.s.PublishMailboxes(ctx, 2)
				requireReceiptCondition(t, n == 0, n, err)
				requireReceiptCondition(t, err != nil, n, err)
			}
			{
				n, err := f.s.PublishMailboxes(ctx, 2)
				requireReceiptCondition(t, n == 1, "healthy next turn", n, err)
				requireReceiptCondition(t, err == nil, "healthy next turn", n, err)
			}
			{
				n := scalar(
					t,
					f.pool,
					`SELECT count(*) FROM fry_work WHERE receipt=$1 AND failures=1 AND `+
						`available_at>clock_timestamp()`,
					receipt.ID.String(),
				)
				requireReceiptCondition(t, n == 1, "poison debt disappeared", n)
			}
		})
	}
}

func TestMailboxOrganizationTimeoutReleasesSelectionGate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a, b := fairnessID(1), fairnessID(2)
	ia, ib := fairnessID(11), fairnessID(12)
	receipts := fairnessQueue(t, f, a, ia, 1)
	fairnessQueue(t, f, b, ib, 1)
	tx, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}()
	_, err = tx.Exec(ctx, `SELECT id FROM fry_receipt WHERE id=$1 FOR UPDATE`, receipts[0].ID.String())
	requireReceiptNoError(t, err)
	blockedCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.s.PublishMailboxes(blockedCtx, 1); done <- err }()
	deadline := time.Now().Add(2 * time.Second)
	for scalar(t, f.pool, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND `+
		`wait_event_type='Lock'`, f.pool.Config().ConnConfig.RuntimeParams["application_name"]) == 0 {
		requireReceiptCondition(t, !time.Now().After(deadline), "publication did not reach held receipt")
		time.Sleep(5 * time.Millisecond)
	}
	// B can finish while A still owns its receipt row lock: selection is separate.
	healthyCtx, healthyCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer healthyCancel()
	{
		n, err := f.s.PublishMailboxes(healthyCtx, 1)
		requireReceiptCondition(t, n == 1, "other organization blocked by selection gate", n, err)
		requireReceiptCondition(t, err == nil, "other organization blocked by selection gate", n, err)
	}
	{
		err := <-done
		requireReceiptCondition(t, err != nil, "held receipt ignored deadline")
	}
	requireReceiptNoError(t, tx.Rollback(ctx))
	{
		n, err := f.s.PublishMailboxes(ctx, 1)
		requireReceiptCondition(t, n == 1, "wrap after timeout", n, err)
		requireReceiptCondition(t, err == nil, "wrap after timeout", n, err)
	}
}

func TestMailboxOrganizationSelectionWaitHonorsContext(t *testing.T) {
	s := &Store{mailboxSelection: make(chan struct{}, 1)}
	s.mailboxSelection <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	{
		n, err := s.PublishMailboxes(ctx, 1)
		requireReceiptCondition(t, n == 0, n, err)
		requireReceiptCondition(t, errors.Is(err, context.DeadlineExceeded), n, err)
	}
	<-s.mailboxSelection
}

func TestMailboxOrganizationFutureBackoffWrapAndNewEligibility(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a, b, c := fairnessID(1), fairnessID(2), fairnessID(3)
	ia, ib, ic := fairnessID(11), fairnessID(12), fairnessID(13)
	fairnessQueue(t, f, a, ia, 1)
	fairnessQueue(t, f, b, ib, 1)
	fairnessQueue(t, f, c, ic, 1)
	execSQL(
		t,
		f.pool,
		`UPDATE fry_work SET available_at=clock_timestamp()+interval '1 hour' WHERE `+
			`organization=$1`,
		a.String(),
	)
	{
		n, err := f.s.PublishMailboxes(ctx, 1)
		requireReceiptCondition(t, n == 1, n, err)
		requireReceiptCondition(t, err == nil, n, err)
	}
	_, mail, err := f.s.Mailbox(ctx, b, ib, "INBOX")
	requireReceiptCondition(t, err == nil, mail, err)
	requireReceiptCondition(t, len(mail) == 1, mail, err)
	execSQL(
		t,
		f.pool,
		`UPDATE fry_work SET available_at=clock_timestamp()-interval '1 second' WHERE `+
			`organization=$1`,
		a.String(),
	)
	// Newly eligible A sorts before cursor B; C still gets the next turn first.
	{
		n, err := f.s.PublishMailboxes(ctx, 1)
		requireReceiptCondition(t, n == 1, n, err)
		requireReceiptCondition(t, err == nil, n, err)
	}
	_, mail, err = f.s.Mailbox(ctx, c, ic, "INBOX")
	requireReceiptCondition(t, err == nil, mail, err)
	requireReceiptCondition(t, len(mail) == 1, mail, err)
	{
		n, err := f.s.PublishMailboxes(ctx, 1)
		requireReceiptCondition(t, n == 1, n, err)
		requireReceiptCondition(t, err == nil, n, err)
	}
	_, mail, err = f.s.Mailbox(ctx, a, ia, "INBOX")
	requireReceiptCondition(t, err == nil, mail, err)
	requireReceiptCondition(t, len(mail) == 1, mail, err)
	{
		n, err := f.s.PublishMailboxes(ctx, 1)
		requireReceiptCondition(t, n == 0, n, err)
		requireReceiptCondition(t, err == nil, n, err)
	}
}

func TestMailboxOrganizationConcurrentAndIndependentStores(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	orgs := []ID{fairnessID(1), fairnessID(2), fairnessID(3)}
	inboxes := []ID{fairnessID(11), fairnessID(12), fairnessID(13)}
	for i := range orgs {
		fairnessQueue(t, f, orgs[i], inboxes[i], 10)
	}
	independent, err := New(ctx, f.pool, f.members, f.s.db.opts)
	requireReceiptNoError(t, err)
	var wg sync.WaitGroup
	var published atomic.Int64
	for worker := range 4 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			store := []*Store{f.s, independent}[index%2]
			for range 12 {
				n, err := store.PublishMailboxes(ctx, 2)
				if err != nil {
					t.Error(err)
					return
				}
				published.Add(int64(n))
			}
		}(worker)
	}
	wg.Wait()
	requireReceiptCondition(t, published.Load() == 30, published.Load())
	{
		n := scalar(t, f.pool, `SELECT count(*) FROM fry_work`)
		requireReceiptCondition(t, n == 0, n)
	}
	requireConcurrentMailboxUIDs(t, ctx, f, orgs, inboxes)
	{
		n := scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_mail m JOIN fry_reference r ON r.receipt=m.receipt AND `+
				`r.kind='mailbox' AND r.reference_key=m.id AND r.expires_at IS NULL`,
		)
		requireReceiptCondition(t, n == 30, "mail/reference mismatch", n)
	}
	{
		n := scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_reference WHERE kind='pending-mailbox' AND (expires_at IS `+
				`NULL OR expires_at>clock_timestamp())`,
		)
		requireReceiptCondition(t, n == 0, "pending ownership retained", n)
	}
}

func TestMailboxOrganizationRetryAvailabilityOrdersTurn(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	org, inbox := fairnessID(1), fairnessID(11)
	receipts := fairnessQueue(t, f, org, inbox, 3)
	execSQL(
		t,
		f.pool,
		`UPDATE fry_work SET available_at=clock_timestamp()+interval '1 hour' WHERE receipt=$1`,
		receipts[0].ID.String(),
	)
	{
		n, err := f.s.PublishMailboxes(ctx, 1)
		requireReceiptCondition(t, n == 1, n, err)
		requireReceiptCondition(t, err == nil, n, err)
	}
	execSQL(t, f.pool, `UPDATE fry_work SET available_at=clock_timestamp() WHERE receipt=$1`, receipts[0].ID.String())
	for range 2 {
		{
			n, err := f.s.PublishMailboxes(ctx, 1)
			requireReceiptCondition(t, n == 1, n, err)
			requireReceiptCondition(t, err == nil, n, err)
		}
	}
	_, mail, err := f.s.Mailbox(ctx, org, inbox, "INBOX")
	requireReceiptCondition(t, err == nil, mail, err)
	requireReceiptCondition(t, len(mail) == 3, mail, err)
	for i, index := range []int{1, 2, 0} {
		requireReceiptCondition(
			t,
			mail[i].Receipt == receipts[index].ID,
			"retry priority changed immutable date or UID allocation",
			i,
			mail[i],
			receipts[index],
		)
		requireReceiptCondition(
			t,
			mail[i].UID == uint32(i+1),
			"retry priority changed immutable date or UID allocation",
			i,
			mail[i],
			receipts[index],
		)
		requireReceiptCondition(
			t,
			mail[i].Date.Equal(receipts[index].ReceivedAt),
			"retry priority changed immutable date or UID allocation",
			i,
			mail[i],
			receipts[index],
		)
	}
}

func requireConcurrentMailboxUIDs(t *testing.T, ctx context.Context, f *fixture, orgs, inboxes []ID) {
	t.Helper()
	for i := range orgs {
		folder, mail, err := f.s.Mailbox(ctx, orgs[i], inboxes[i], "INBOX")
		requireReceiptCondition(t, err == nil, folder, len(mail), err)
		requireReceiptCondition(t, len(mail) == 10, folder, len(mail), err)
		requireReceiptCondition(t, folder.UIDNext == 11, folder, len(mail), err)
		seen := map[ID]bool{}
		for j, m := range mail {
			requireReceiptCondition(t, m.UID == uint32(j+1), "duplicate or UID gap", m)
			requireReceiptCondition(t, !seen[m.Receipt], "duplicate or UID gap", m)
			seen[m.Receipt] = true
		}
	}
}
