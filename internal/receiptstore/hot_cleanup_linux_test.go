//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestHotRewriteWithoutS3PreservesLiveReplicaAndRemovesDeadBytes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	dead := accept(t, f.s, newID(t), f.envelope(t), "discard-this-private-document")
	live := accept(t, f.s, newID(t), f.envelope(t), "preserve-live-document")
	expireReceipt(t, f, dead)
	for i, m := range f.members {
		old := m.bytes.FileID()
		requireReceiptNoError(t, f.s.ReclaimHot(ctx, m.id))
		requireReceiptCondition(t, m.bytes.FileID() != old, "not rewritten", m.bytes.Inventory())
		requireReceiptCondition(t, len(m.bytes.Inventory()) == 1, "not rewritten", m.bytes.Inventory())
		{
			_, err := os.Stat(filepath.Join(f.dirs[i], "data.fry"))
			requireReceiptCondition(t, errors.Is(err, os.ErrNotExist), "old body file remains", err)
		}
	}
	var out bytes.Buffer
	{
		err := f.s.Read(ctx, f.org, live.ID, 0, live.Size, &out)
		requireReceiptCondition(t, err == nil, "survivor damaged", err)
		requireReceiptCondition(t, out.String() == "preserve-live-document", "survivor damaged", err)
	}
	{
		n, err := f.s.FinishCleanup(ctx, 100)
		requireReceiptCondition(t, err == nil, n, err)
		requireReceiptCondition(t, n == 1, n, err)
	}
	status, err := f.s.CleanupStatus(ctx, f.org, dead.ID)
	requireReceiptCondition(t, err == nil, status, err)
	requireReceiptCondition(t, status.State == "removed", status, err)
	requireReceiptCondition(t, status.HotCopies == 0, status, err)
	requireReceiptCondition(t, !status.Overdue, status, err)
	// New reception uses the new incarnation rather than reusing retired files.
	accept(t, f.s, newID(t), f.envelope(t), "later original")
	for i, m := range f.members {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
		reopened, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, f.dirs[i], hotOptions())
		requireReceiptNoError(t, err)
		t.Cleanup(func() {
			if err := reopened.Close(); err != nil {
				t.Error(err)
			}
		})
		f.members[i] = reopened
	}
	reopened, err := New(ctx, f.pool, f.members, f.s.db.opts)
	requireReceiptNoError(t, err)
	out.Reset()
	{
		err = reopened.Read(ctx, f.org, live.ID, 0, live.Size, &out)
		requireReceiptCondition(t, err == nil, "reopen lost successor", err)
		requireReceiptCondition(t, out.String() == "preserve-live-document", "reopen lost successor", err)
	}
}

func TestHotEvictionKeepsPublishedColdMailAccessible(t *testing.T) {
	f, p, _, _ := packing(t)
	ctx := context.Background()
	r := accept(t, f.s, newID(t), f.envelope(t), "cold but still visible")
	{
		_, err := p.Pack(ctx, f.org, []ID{r.ID})
		requireReceiptCondition(t, err == nil, err)
	}
	for _, m := range f.members {
		requireReceiptNoError(t, f.s.ReclaimHot(ctx, m.id))
		requireReceiptCondition(t, len(m.bytes.Inventory()) == 0, "packed hot bytes retained")
	}
	var out bytes.Buffer
	{
		err := f.s.Read(ctx, f.org, r.ID, 0, r.Size, &out)
		requireReceiptCondition(t, err == nil, err)
		requireReceiptCondition(t, out.String() == "cold but still visible", err)
	}
	status, err := f.s.CleanupStatus(ctx, f.org, r.ID)
	requireReceiptCondition(t, err == nil, status, err)
	requireReceiptCondition(t, status.State == "live", status, err)
	requireReceiptCondition(t, status.HotCopies == 0, status, err)
	requireReceiptCondition(t, status.Packs == 1, status, err)
}

func TestHotRewriteRecoveryBeforeAndAfterPublication(t *testing.T) {
	for _, point := range []string{"after_copy", "after_publish"} {
		t.Run(point, func(t *testing.T) {
			testHotRewriteRecoveryCut(t, point)
		})
	}
}

func TestPreparingTargetsBlockReclamationAndAbortedCopiesAreAccounted(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	attempt := newID(t)
	claim := prepare(t, f, attempt, f.envelope(t))
	copies(t, f, claim, "abandoned bytes")
	{
		err := f.s.ReclaimHot(ctx, f.members[0].id)
		requireReceiptCondition(t, errors.Is(err, ErrBusy), "preparing target reclaimed", err)
	}
	requireReceiptNoError(t, f.s.Abort(ctx, f.org, attempt))
	for _, m := range f.members {
		requireReceiptNoError(t, f.s.ReclaimHot(ctx, m.id))
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_cleanup WHERE bytes_removed") == 2,
		"abandoned bytes not accounted",
	)
}

func TestFinalCleanupWaitsForPackStagingAndBothHotCopies(t *testing.T) {
	f, p, _, _ := packing(t)
	ctx := context.Background()
	r := accept(t, f.s, newID(t), f.envelope(t), "erase all active copies")
	pack, err := p.Pack(ctx, f.org, []ID{r.ID})
	requireReceiptNoError(t, err)
	expireReceipt(t, f, r)
	_, err = p.Compact(ctx, f.org, pack)
	requireReceiptNoError(t, err)
	err = p.Retire(ctx, f.org, pack)
	requireReceiptNoError(t, err)
	err = f.s.ReclaimHot(ctx, f.members[0].id)
	requireReceiptNoError(t, err)
	{
		n, err := f.s.FinishCleanup(ctx, 100)
		requireReceiptCondition(t, err == nil, "offline copy forgotten", n, err)
		requireReceiptCondition(t, n == 0, "offline copy forgotten", n, err)
	}
	err = f.s.ReclaimHot(ctx, f.members[1].id)
	requireReceiptNoError(t, err)
	execSQL(t, f.pool, "UPDATE fry_pack SET staging_closed=false WHERE id=$1", pack.String())
	{
		n, err := f.s.FinishCleanup(ctx, 100)
		requireReceiptCondition(t, err == nil, "unresolved staging forgotten", n, err)
		requireReceiptCondition(t, n == 0, "unresolved staging forgotten", n, err)
	}
	execSQL(t, f.pool, "UPDATE fry_pack SET staging_closed=true WHERE id=$1", pack.String())
	{
		n, err := f.s.FinishCleanup(ctx, 100)
		requireReceiptCondition(t, err == nil, n, err)
		requireReceiptCondition(t, n == 1, n, err)
	}
}

func testHotRewriteRecoveryCut(t *testing.T, point string) {
	t.Helper()

	f := setup(t)
	ctx := context.Background()
	dead := accept(t, f.s, newID(t), f.envelope(t), "old expired original")
	live := accept(t, f.s, newID(t), f.envelope(t), "surviving original")
	expireReceipt(t, f, dead)
	m := f.members[0]
	before := m.bytes.FileID()
	m.rewriteFault = func(at string) error {
		if at == point {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	{
		err := f.s.ReclaimHot(ctx, m.id)
		requireReceiptCondition(t, errors.Is(err, io.ErrUnexpectedEOF), err)
	}
	if err := m.Close(); err != nil {
		t.Error(err)
	}
	next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, f.dirs[0], hotOptions())
	requireReceiptNoError(t, err)
	t.Cleanup(func() {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
	})
	f.members[0] = next
	requireReceiptCondition(t, point != "after_copy" || next.bytes.FileID() == before, "unpublished file selected")
	requireReceiptCondition(t, point != "after_publish" || next.bytes.FileID() != before, "published file forgotten")
	f.s, err = New(ctx, f.pool, f.members, f.s.db.opts)
	requireReceiptNoError(t, err)
	if err := f.members[1].Close(); err != nil {
		t.Error(err)
	} // Require the recovered member's bytes.
	var out bytes.Buffer
	{
		err = f.s.Read(ctx, f.org, live.ID, 0, live.Size, &out)
		requireReceiptCondition(t, err == nil, err)
		requireReceiptCondition(t, out.String() == "surviving original", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_hot_rewrite WHERE state IN ('building','published')") == 0,
		"unreconciled rewrite",
	)
}
