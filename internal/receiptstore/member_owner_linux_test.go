//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemberOwnerCloseCancelsBeforeWaitingAndRecordsProofAfterDrain(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	m := f.members[0]
	o, err := ownMember(ctx, m)
	requireReceiptNoError(t, err)
	t.Cleanup(func() {
		if err := o.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	{
		_, err := ownMember(ctx, m)
		requireReceiptCondition(t, errors.Is(err, ErrBusy), "two owners admitted", err)
	}
	work, finished, err := o.begin(ctx)
	requireReceiptNoError(t, err)
	defer finished()
	deadline, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	{
		err := o.Close(deadline)
		requireReceiptCondition(t, errors.Is(err, context.DeadlineExceeded), "did not wait for storage", err)
	}
	select {
	case <-work.Done():
	default:
		t.Fatal("close waited before canceling operations")
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member_owner WHERE member=$1 AND closed`, m.id.String()) == 0,
		"claimed closure before handles stopped",
	)
	finished()
	requireReceiptNoError(t, o.Close(ctx))
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member_owner WHERE member=$1 AND closed`, m.id.String()) == 1,
		"missing closed generation",
	)
	{
		_, _, err := o.begin(ctx)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "closed owner admitted work", err)
	}
	{
		_, err := ownMember(ctx, m)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "closed byte member admitted", err)
	}
	requireNextOwnerGeneration(t, ctx, f, m, o)
}

func TestMemberOwnerLostSessionDoesNotAuthorizeTakeover(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	m := f.members[0]
	o, err := ownMember(ctx, m)
	requireReceiptNoError(t, err)
	t.Cleanup(func() {
		if err := o.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	work, finished, err := o.begin(ctx)
	requireReceiptNoError(t, err)
	defer finished()
	var terminated bool
	{
		err := f.pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, int64(o.connection.PgConn().PID())).
			Scan(&terminated)
		requireReceiptCondition(t, err == nil, terminated, err)
		requireReceiptCondition(t, terminated, terminated, err)
	}
	select {
	case <-work.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("lost DB ownership did not cancel work")
	}
	{
		_, _, err := o.begin(ctx)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "lost owner admitted work", err)
	}
	// PostgreSQL released the advisory lock, but the prior writer is still an
	// unclosed obligation. A new process must not invent cancellation proof.
	{
		_, err := ownMember(ctx, m)
		requireReceiptCondition(t, errors.Is(err, ErrBusy), "advisory lock mistaken for fencing", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member_owner WHERE member=$1 AND NOT closed`, m.id.String()) == 1,
		"lost generation discarded",
	)
	finished()
	requireReceiptNoError(t, o.Close(ctx))
}

func requireNextOwnerGeneration(t *testing.T, ctx context.Context, f *fixture, m *LocalMember, o *memberOwner) {
	t.Helper()
	reopened, err := OpenLocalMember(context.WithoutCancel(ctx), f.pool, m.id, m.domain, f.dirs[0], hotOptions())
	requireReceiptNoError(t, err)
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	next, err := ownMember(ctx, reopened)
	requireReceiptNoError(t, err)
	defer func() {
		if err := next.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	requireReceiptCondition(t, next.epoch == o.epoch+1, "generation reused")
	tx, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer rollback(context.WithoutCancel(ctx), tx)
	requireReceiptNoError(t, next.guard(ctx, tx))
	stale := &memberOwner{member: reopened, epoch: o.epoch, instance: o.instance, ctx: ctx}
	{
		err := stale.guard(ctx, tx)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "old generation passed guard", err)
	}
	rollback(context.WithoutCancel(ctx), tx)
}
