//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func expireReceipt(t *testing.T, f *fixture, r Receipt) {
	t.Helper()
	execSQL(
		t,
		f.pool,
		`UPDATE fry_reference SET expires_at=clock_timestamp()-interval '2 days' WHERE `+
			`receipt=$1`,
		r.ID.String(),
	)
	{
		_, err := f.s.SweepExpiry(context.Background(), 100)
		requireReceiptCondition(t, err == nil, err)
	}
}

func TestReferenceTransferAndReceiptBasedRecovery(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	env := f.envelope(t)
	r := accept(t, f.s, newID(t), env, "original")
	// A visible mailbox outlives the original recovery window.
	execSQL(
		t,
		f.pool,
		`UPDATE fry_reference SET expires_at=clock_timestamp()-interval '1 day' WHERE `+
			`receipt=$1 AND kind='recovery'`,
		r.ID.String(),
	)
	ref := Reference{Kind: "mailbox", Key: "stable-membership"}
	requireReceiptNoError(t, f.s.AddReference(ctx, f.org, r.ID, ref))
	{
		err := f.s.AddReference(ctx, f.org, r.ID, ref)
		requireReceiptCondition(t, err == nil, "idempotent add", err)
	}
	pending := Reference{Kind: "pending-mailbox", Key: env.Destinations[0].Inbox.String()}
	requireReceiptNoError(t, f.s.ReleaseReference(ctx, f.org, r.ID, pending))
	{
		n, err := f.s.SweepExpiry(ctx, 100)
		requireReceiptCondition(t, err == nil, "visible mail expired", n, err)
		requireReceiptCondition(t, n == 0, "visible mail expired", n, err)
	}
	var body bytes.Buffer
	{
		err := f.s.Read(ctx, f.org, r.ID, 0, r.Size, &body)
		requireReceiptCondition(t, err == nil, err)
		requireReceiptCondition(t, body.String() == "original", err)
	}
	requireReceiptNoError(t, f.s.ReleaseReference(ctx, f.org, r.ID, ref))
	first, err := f.s.CleanupStatus(ctx, f.org, r.ID)
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, first.State == "retiring", first)
	requireReceiptCondition(t, first.EligibleAt != nil, first)
	requireReceiptCondition(t, time.Since(*first.EligibleAt) <= time.Minute, first)
	requireReceiptCondition(t, first.HotCopies == 2, first)
	err = f.s.ReleaseReference(ctx, f.org, r.ID, ref)
	requireReceiptNoError(t, err)
	second, err := f.s.CleanupStatus(ctx, f.org, r.ID)
	requireReceiptCondition(t, err == nil, "deadline moved", first, second, err)
	requireReceiptCondition(t, first.EligibleAt.Equal(*second.EligibleAt), "deadline moved", first, second, err)
	requireReceiptCondition(t, first.Due.Equal(*second.Due), "deadline moved", first, second, err)
	{
		_, err = f.s.Open(ctx, f.org, r.ID, 0, r.Size)
		requireReceiptCondition(t, errors.Is(err, ErrExpired), "expired original exposed", err)
	}
	{
		err = f.s.AddReference(ctx, f.org, r.ID, Reference{Kind: "mailbox", Key: "resurrection"})
		requireReceiptCondition(t, errors.Is(err, ErrExpired), "receipt revived", err)
	}
	{
		status, err := f.s.Status(ctx, f.org, r.Attempt)
		requireReceiptCondition(t, err == nil, "acceptance history lost", status, err)
		requireReceiptCondition(t, status.State == "accepted", "acceptance history lost", status, err)
	}
	{
		_, err = f.s.CleanupStatus(ctx, newID(t), r.ID)
		requireReceiptCondition(t, errors.Is(err, ErrNotFound), "org isolation", err)
	}
}

func TestReleasedReferenceCannotBeRecreated(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r := accept(t, f.s, newID(t), f.envelope(t), "body")
	ref := Reference{Kind: "delivery", Key: "app-delivery"}
	requireReceiptNoError(t, f.s.AddReference(ctx, f.org, r.ID, ref))
	requireReceiptNoError(t, f.s.ReleaseReference(ctx, f.org, r.ID, ref))
	{
		err := f.s.AddReference(ctx, f.org, r.ID, ref)
		requireReceiptCondition(t, errors.Is(err, ErrConflict), "delayed add revived completed delivery", err)
	}
	{
		err := f.s.ReleaseReference(ctx, f.org, r.ID, Reference{Kind: "recovery", Key: "original"})
		requireReceiptCondition(t, err != nil, "caller shortened recovery retention")
	}
}

func TestDelayedSweepPreservesNaturalExpiryDeadline(t *testing.T) {
	f := setup(t)
	r := accept(t, f.s, newID(t), f.envelope(t), "body")
	expireReceipt(t, f, r)
	status, err := f.s.CleanupStatus(context.Background(), f.org, r.ID)
	requireReceiptCondition(t, err == nil, "sweep reset cleanup clock", status, err)
	requireReceiptCondition(t, status.EligibleAt != nil, "sweep reset cleanup clock", status, err)
	requireReceiptCondition(t, time.Since(*status.EligibleAt) >= 47*time.Hour, "sweep reset cleanup clock", status, err)
	requireReceiptCondition(t, status.Overdue, "sweep reset cleanup clock", status, err)
	requireReceiptCondition(t, status.Due.Sub(*status.EligibleAt) == 24*time.Hour, status)
}

func TestUnreadHandleDeadlineClosesStorageBeforeDroppingPin(t *testing.T) {
	f := setup(t)
	f.s.db.opts.MaxReadLifetime = 75 * time.Millisecond
	r := accept(t, f.s, newID(t), f.envelope(t), strings.Repeat("x", 100000))
	handle, err := f.s.Open(context.Background(), f.org, r.ID, 0, r.Size)
	requireReceiptNoError(t, err)
	defer func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-handle.done:
	case <-time.After(3 * time.Second):
		t.Fatal("unconsumed reader did not cancel")
	}
	{
		_, err = io.ReadAll(handle)
		requireReceiptCondition(t, errors.Is(err, context.DeadlineExceeded), "timeout not returned", err)
	}
	requireReceiptCondition(t, scalar(t, f.pool, "SELECT count(*) FROM fry_read_pin") == 0, "completed read left pin")
}

func TestPackCompactionPreservesPinnedOldViewAndLiveRecord(t *testing.T) {
	f, p, server, _ := packing(t)
	ctx := context.Background()
	dead := accept(t, f.s, newID(t), f.envelope(t), "expired business document")
	body := strings.Repeat("surviving original\r\n", 10000)
	live := accept(t, f.s, newID(t), f.envelope(t), body)
	old, err := p.Pack(ctx, f.org, []ID{dead.ID, live.ID})
	requireReceiptNoError(t, err)
	for _, m := range f.members {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	}
	reader, err := f.s.Open(ctx, f.org, live.ID, 0, live.Size)
	requireReceiptNoError(t, err)
	defer func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	}()
	expireReceipt(t, f, dead)
	next, err := p.Compact(ctx, f.org, old)
	requireReceiptCondition(t, err == nil, next, err)
	requireReceiptCondition(t, next != (ID{}), next, err)
	{
		state, err := p.Status(ctx, f.org, old)
		requireReceiptCondition(t, err == nil, state, err)
		requireReceiptCondition(t, state == "retiring", state, err)
	}
	{
		err = p.Retire(ctx, f.org, old)
		requireReceiptCondition(t, errors.Is(err, ErrBusy), "retired while old reader active", err)
	}
	got, err := io.ReadAll(reader)
	if err := reader.Close(); err != nil {
		t.Error(err)
	}
	requireReceiptCondition(t, err == nil, "old pinned view changed", err)
	requireReceiptCondition(t, string(got) == body, "old pinned view changed", err)
	err = p.Retire(ctx, f.org, old)
	requireReceiptNoError(t, err)
	{
		obj, _ := server.Object(objectKey(t, f, old))
		requireReceiptCondition(t, obj.Retired, "old pack bytes retained")
		requireReceiptCondition(t, len(obj.Body) == 0, "old pack bytes retained")
	}
	requireCompactedReceiptLifecycle(t, ctx, f, live, dead, body)
}

func TestAllDeadPackRetiresWithoutReplacementAndExpiredPinStillBlocks(t *testing.T) {
	f, p, _, _ := packing(t)
	ctx := context.Background()
	r := accept(t, f.s, newID(t), f.envelope(t), "body")
	old, err := p.Pack(ctx, f.org, []ID{r.ID})
	requireReceiptNoError(t, err)
	// Simulates a partitioned/crashed owner's unresolved handle. Its elapsed
	// deadline cannot authorize physical deletion without owner reconciliation.
	pin := newID(t)
	execSQL(
		t,
		f.pool,
		`INSERT INTO fry_read_pin(id,receipt,pack,deadline) `+
			`VALUES($1,$2,$3,clock_timestamp()-interval '1 hour')`,
		pin.String(),
		r.ID.String(),
		old.String(),
	)
	expireReceipt(t, f, r)
	next, err := p.Compact(ctx, f.org, old)
	requireReceiptCondition(t, err == nil, next, err)
	requireReceiptCondition(t, next == (ID{}), next, err)
	{
		err = p.Retire(ctx, f.org, old)
		requireReceiptCondition(t, errors.Is(err, ErrBusy), "expired pin treated as closed handle", err)
	}
	execSQL(
		t,
		f.pool,
		`DELETE FROM fry_read_pin WHERE id=$1`,
		pin.String(),
	) // fixture simulates verified owner completion
	err = p.Retire(ctx, f.org, old)
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, scalar(t, f.pool, "SELECT count(*) FROM fry_pack") == 1, "empty replacement uploaded")
}

func TestCompactionRejectsMembershipExpiryDuringUpload(t *testing.T) {
	f, p, _, _ := packing(t)
	ctx := context.Background()
	dead := accept(t, f.s, newID(t), f.envelope(t), "dead")
	live := accept(t, f.s, newID(t), f.envelope(t), "survivor until upload")
	old, err := p.Pack(ctx, f.org, []ID{dead.ID, live.ID})
	requireReceiptNoError(t, err)
	expireReceipt(t, f, dead)
	p.afterUpload = func(ID) error { expireReceipt(t, f, live); return nil }
	failed, err := p.Compact(ctx, f.org, old)
	requireReceiptCondition(t, errors.Is(err, ErrStale), "published stale rewrite", err)
	{
		state, err := p.Status(ctx, f.org, old)
		requireReceiptCondition(t, err == nil, "source retired on failed swap", state, err)
		requireReceiptCondition(t, state == "published", "source retired on failed swap", state, err)
	}
	err = p.Retire(ctx, f.org, failed)
	requireReceiptNoError(t, err)
	p.afterUpload = nil
	_, err = p.Compact(ctx, f.org, old)
	requireReceiptNoError(t, err)
	err = p.Retire(ctx, f.org, old)
	requireReceiptNoError(t, err)
}

func requireCompactedReceiptLifecycle(t *testing.T, ctx context.Context, f *fixture, live, dead Receipt, body string) {
	t.Helper()
	var err error
	var out bytes.Buffer
	{
		err = f.s.Read(ctx, f.org, live.ID, 0, live.Size, &out)
		requireReceiptCondition(t, err == nil, "surviving cold data lost", err)
		requireReceiptCondition(t, out.String() == body, "surviving cold data lost", err)
	}
	{
		_, err = f.s.Open(ctx, f.org, dead.ID, 0, dead.Size)
		requireReceiptCondition(t, errors.Is(err, ErrExpired), "expired record readable", err)
	}
	debt, err := f.s.CleanupStatus(ctx, f.org, dead.ID)
	requireReceiptCondition(t, err == nil, "falsely declared erasure", debt, err)
	requireReceiptCondition(t, debt.Packs == 0, "falsely declared erasure", debt, err)
	requireReceiptCondition(t, debt.HotCopies == 2, "falsely declared erasure", debt, err)
	requireReceiptCondition(t, debt.State == "retiring", "falsely declared erasure", debt, err)
}
