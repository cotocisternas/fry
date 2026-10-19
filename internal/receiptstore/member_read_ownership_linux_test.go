//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
)

// readOrder returns local members in the Store's deterministic read order.
func readOrder(f *fixture) (first, second *LocalMember) {
	first, second = f.members[0], f.members[1]
	if second.id.String() < first.id.String() {
		first, second = second, first
	}
	return first, second
}

// waitForQueuedGateWriter observes an exclusive gate request queued behind
// readers: a pending writer makes every new shared acquisition fail.
func waitForQueuedGateWriter(t *testing.T, m *LocalMember) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !m.gate.TryRLock() {
			return
		}
		m.gate.RUnlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("exclusive gate request did not queue")
}

func waitForReadPermits(t *testing.T, m *LocalMember, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(m.readPermits) != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	requireReceiptCondition(t, len(m.readPermits) == want, "read permits", len(m.readPermits), want)
}

func countImageCloses(m *LocalMember) *atomic.Int32 {
	var closes atomic.Int32
	m.imageClose = func(image *hotstore.Image) error { closes.Add(1); return image.Close() }
	return &closes
}

func TestReadUnusedMemberGateDoesNotBlockSelectedMember(t *testing.T) {
	f, live, body := sealedReclaimReaderFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	first, second := readOrder(f)
	old, path, _ := sealedReclaimSourcePath(t, f, first, live)
	// Models exclusive maintenance holding the replica this read never selects.
	second.gate.Lock()
	unlockSecond := sync.OnceFunc(second.gate.Unlock)
	defer unlockSecond()
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() { done <- f.s.Read(ctx, f.org, live.ID, 0, live.Size, &out) }()
	select {
	case err := <-done:
		requireReceiptCondition(t, err == nil, "healthy selected member read failed", err)
		requireReceiptCondition(t, bytes.Equal(out.Bytes(), body), "healthy selected member read differs")
	case <-time.After(5 * time.Second):
		unlockSecond()
		t.Fatal("unused member gate blocked healthy selected member")
	}
	// The selected member's own descriptor still excludes its reclamation.
	h, err := f.s.Open(ctx, f.org, live.ID, 0, live.Size)
	requireReceiptNoError(t, err)
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	prefix := make([]byte, 16)
	{
		_, err = io.ReadFull(h, prefix)
		requireReceiptCondition(t, err == nil, err)
		requireReceiptCondition(t, bytes.Equal(prefix, body[:16]), err)
	}
	reclaimed := make(chan error, 1)
	go func() { reclaimed <- f.s.ReclaimSealed(ctx, first.id) }()
	select {
	case err = <-reclaimed:
		t.Fatal("sealed reclaim passed the selected member's live descriptor", err)
	case <-time.After(50 * time.Millisecond):
	}
	{
		_, err = os.Stat(path)
		requireReceiptCondition(t, err == nil, "source removed while descriptor active", err)
	}
	requireReceiptNoError(t, h.Close())
	select {
	case err = <-reclaimed:
		requireReceiptNoError(t, err)
	case <-ctx.Done():
		t.Fatal("reclaim did not resume after descriptor closed", ctx.Err())
	}
	unlockSecond()
	requireSealedSourceReclaimed(t, f, first, old, path, live, body)
	requireReceiptCondition(t, scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin`) == 0, "reads retained pins")
}

func receiveWithin[T any](t *testing.T, ctx context.Context, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatal(what, ctx.Err())
		var zero T
		return zero
	}
}

func requirePending[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatal(what, v)
	case <-time.After(50 * time.Millisecond):
	}
}

// openIdleReaders fills m's permits with real reads whose consumers are idle.
// Each holds its permit, the shared gate and a sealed descriptor.
func openIdleReaders(t *testing.T, ctx context.Context, f *fixture, m *LocalMember, r Receipt) []*ReadHandle {
	t.Helper()
	var handles []*ReadHandle
	t.Cleanup(func() {
		for _, h := range handles {
			if err := h.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	for range cap(m.readPermits) {
		h, err := f.s.Open(ctx, f.org, r.ID, 0, r.Size)
		requireReceiptNoError(t, err)
		handles = append(handles, h)
	}
	waitForReadPermits(t, m, cap(m.readPermits))
	return handles
}

func requireCancellablePermitWait(t *testing.T, ctx context.Context, f *fixture, r Receipt) {
	t.Helper()
	waiting, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	h, err := f.s.Open(waiting, f.org, r.ID, 0, r.Size)
	requireReceiptNoError(t, err)
	_, err = io.Copy(io.Discard, h)
	requireReceiptCondition(t, errors.Is(err, context.DeadlineExceeded), "permit wait ignored context", err)
	requireReceiptNoError(t, h.Close())
}

func readAllAsync(t *testing.T, h *ReadHandle) <-chan []byte {
	t.Helper()
	done := make(chan []byte, 1)
	go func() {
		b, err := io.ReadAll(h)
		if err != nil {
			t.Error(err)
		}
		done <- b
	}()
	return done
}

func TestReadPermitWaitCancelsWhileGateWaitFollowsQueuedMaintenance(t *testing.T) {
	f, live, body := sealedReclaimReaderFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, _ := readOrder(f)
	old, path, _ := sealedReclaimSourcePath(t, f, first, live)
	handles := openIdleReaders(t, ctx, f, first, live)
	reclaimed := make(chan error, 1)
	go func() { reclaimed <- f.s.ReclaimSealed(ctx, first.id) }()
	waitForQueuedGateWriter(t, first)
	requireCancellablePermitWait(t, ctx, f, live)
	// Two freed permits admit readers that then queue behind maintenance.
	for _, h := range handles[:2] {
		requireReceiptNoError(t, h.Close())
	}
	canceled, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	abandoned, err := f.s.Open(canceled, f.org, live.ID, 0, live.Size)
	requireReceiptNoError(t, err)
	queued, err := f.s.Open(ctx, f.org, live.ID, 0, live.Size)
	requireReceiptNoError(t, err)
	queuedRead := readAllAsync(t, queued)
	waitForReadPermits(t, first, cap(first.readPermits))
	cancelRead()
	abandonedClosed := make(chan error, 1)
	go func() { abandonedClosed <- abandoned.Close() }()
	requirePending(t, abandonedClosed, "gate wait became cancellable")
	requirePending(t, reclaimed, "maintenance passed active descriptors")
	requireReceiptCondition(t, len(first.readPermits) == cap(first.readPermits), "canceled waiter released permit")
	for _, h := range handles[2:] {
		requireReceiptNoError(t, h.Close())
	}
	requireReceiptNoError(t, receiveWithin(t, ctx, reclaimed, "maintenance did not run after descriptors closed"))
	requireReceiptNoError(t, receiveWithin(t, ctx, abandonedClosed, "canceled gate waiter did not finish"))
	b := receiveWithin(t, ctx, queuedRead, "queued reader did not finish")
	requireReceiptCondition(t, bytes.Equal(b, body), "queued reader did not resolve successor", len(b))
	requireReceiptNoError(t, queued.Close())
	requireSealedSourceReclaimed(t, f, first, old, path, live, body)
	requireReceiptCondition(t, len(first.readPermits) == 0, "readers retained permits")
	requireReceiptCondition(t, scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin`) == 0, "reads retained pins")
}

func TestReadMemberCloseBeforeBytesFallsBackToReplica(t *testing.T) {
	f := sealedReadFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	body := sealedReadBody()
	r := accept(t, f.s, newID(t), f.envelope(t), string(body))
	for _, m := range f.members {
		requireReceiptNoError(t, f.s.RotateHot(ctx, m.id))
	}
	first, second := readOrder(f)
	replicaCloses := countImageCloses(second)
	h, err := f.s.Open(ctx, f.org, r.ID, 0, r.Size)
	requireReceiptNoError(t, err)
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	prefix := make([]byte, 16)
	_, err = io.ReadFull(h, prefix)
	requireReceiptNoError(t, err)
	closed := make(chan error, 1)
	go func() { closed <- first.Close() }()
	waitForQueuedGateWriter(t, first)
	read := make(chan error, 1)
	var out bytes.Buffer
	go func() { read <- f.s.Read(ctx, f.org, r.ID, 0, r.Size, &out) }()
	waitForReadPermits(t, first, 2)
	requireReceiptNoError(t, h.Close())
	select {
	case err = <-closed:
		requireReceiptNoError(t, err)
	case <-ctx.Done():
		t.Fatal("member close did not finish", ctx.Err())
	}
	select {
	case err = <-read:
		requireReceiptCondition(t, err == nil, "closed member did not fall back before bytes", err)
		requireReceiptCondition(t, bytes.Equal(out.Bytes(), body), "fallback read differs", out.Len())
	case <-ctx.Done():
		t.Fatal("queued read did not finish", ctx.Err())
	}
	requireReceiptCondition(t, replicaCloses.Load() == 1, "replica did not serve fallback", replicaCloses.Load())
	requireReceiptCondition(t, scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin`) == 0, "reads retained pins")
}

// cancelingWriter cancels its read after the first accepted bytes. It has no
// ReadFrom method, so io.Copy delivers every chunk through Write.
type cancelingWriter struct {
	out    bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelingWriter) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	w.cancel()
	return n, err
}

func TestReadCancellationAfterBytesDoesNotFallBack(t *testing.T) {
	f := sealedReadFixture(t)
	ctx := context.Background()
	body := sealedReadBody()
	r := accept(t, f.s, newID(t), f.envelope(t), string(body))
	for _, m := range f.members {
		requireReceiptNoError(t, f.s.RotateHot(ctx, m.id))
	}
	first, second := readOrder(f)
	selectedCloses, replicaCloses := countImageCloses(first), countImageCloses(second)
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &cancelingWriter{cancel: cancel}
	err := f.s.Read(readCtx, f.org, r.ID, 0, r.Size, out)
	requireReceiptCondition(t, errors.Is(err, context.Canceled), "canceled read reported", err)
	n := out.out.Len()
	requireReceiptCondition(t, n > 0 && n < len(body), "canceled read length", n)
	requireReceiptCondition(t, bytes.Equal(out.out.Bytes(), body[:n]), "canceled read emitted foreign bytes")
	requireReceiptCondition(t, selectedCloses.Load() == 1, "selected descriptor not closed", selectedCloses.Load())
	requireReceiptCondition(t, replicaCloses.Load() == 0, "canceled read fell back after bytes")
	requireReceiptCondition(t, scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin`) == 0, "read retained pins")
}

func TestReadMixedLocalRemotePairServesFromEitherAdapter(t *testing.T) {
	for _, survivor := range []string{"local", "remote"} {
		t.Run(survivor, func(t *testing.T) {
			f := sealedReadFixture(t)
			ctx := context.Background()
			local := f.members[0]
			remote, server := remoteMember(t, f, 1)
			s, err := NewWithMembers(ctx, f.pool, [2]Member{local, remote}, f.s.db.opts)
			requireReceiptNoError(t, err)
			body := sealedReadBody()
			r := accept(t, s, newID(t), f.envelope(t), string(body))
			requireSealedRead(t, s, f.org, r, body)
			for _, m := range f.members {
				requireReceiptNoError(t, s.RotateHot(ctx, m.id))
			}
			fresh := accept(t, s, newID(t), f.envelope(t), "mixed current original")
			if survivor == "local" {
				requireReceiptNoError(t, server.Close(ctx))
			} else {
				requireReceiptNoError(t, local.Close())
			}
			requireSealedRead(t, s, f.org, r, body)
			var out bytes.Buffer
			{
				err = s.Read(ctx, f.org, fresh.ID, 0, fresh.Size, &out)
				requireReceiptCondition(t, err == nil, "current read through survivor failed", err)
				requireReceiptCondition(t, out.String() == "mixed current original", "current read differs", err)
			}
			if survivor == "remote" {
				requireReceiptCondition(
					t,
					scalar(
						t,
						f.pool,
						`SELECT count(*) FROM fry_member_read WHERE member=$1 AND closed`,
						f.members[1].id.String(),
					) >= 4,
					"remote adapter did not serve with closure proof",
				)
			}
			requireReceiptCondition(
				t,
				scalar(t, f.pool, `SELECT count(*) FROM fry_member_read WHERE NOT closed`) == 0,
				"mixed read retained member pin debt",
			)
			requireReceiptCondition(
				t,
				scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin`) == 0,
				"mixed read retained pins",
			)
		})
	}
}

func TestReadMalformedLocatorAffectsOnlyItsOwnMember(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r := accept(t, f.s, newID(t), f.envelope(t), "replicated original")
	first, second := readOrder(f)
	locators := map[ID][]byte{}
	for _, m := range f.members {
		var raw []byte
		requireReceiptNoError(t, f.pool.QueryRow(
			ctx,
			`SELECT locator FROM fry_target WHERE attempt=$1 AND member=$2`,
			r.Attempt.String(),
			m.id.String(),
		).Scan(&raw))
		locators[m.id] = raw
	}
	setLocator := func(m *LocalMember, raw []byte) {
		t.Helper()
		execSQL(
			t,
			f.pool,
			`UPDATE fry_target SET locator=$3::jsonb WHERE attempt=$1 AND member=$2`,
			r.Attempt.String(),
			m.id.String(),
			string(raw),
		)
	}
	malformed := []byte(`"malformed"`)
	var attempts []ID
	f.s.beforeReadAttempt = func(member ID) { attempts = append(attempts, member) }
	read := func() (string, error) {
		t.Helper()
		attempts = nil
		var out bytes.Buffer
		err := f.s.Read(ctx, f.org, r.ID, 0, r.Size, &out)
		return out.String(), err
	}

	setLocator(second, malformed)
	{
		got, err := read()
		requireReceiptCondition(t, err == nil, "unused malformed locator blocked read", err)
		requireReceiptCondition(t, got == "replicated original", "unused malformed locator read differs", got)
		requireReceiptCondition(t, len(attempts) == 1 && attempts[0] == first.id, "unexpected attempts", attempts)
	}
	setLocator(second, locators[second.id])
	setLocator(first, malformed)
	{
		got, err := read()
		requireReceiptCondition(t, err == nil, "selected malformed locator did not fall back", err)
		requireReceiptCondition(t, got == "replicated original", "fallback read differs", got)
		requireReceiptCondition(t, len(attempts) == 2 && attempts[1] == second.id, "no fallback attempt", attempts)
	}
	setLocator(second, malformed)
	{
		got, err := read()
		requireReceiptCondition(t, err != nil, "read succeeded without a valid locator")
		requireReceiptCondition(t, got == "", "read emitted bytes without a valid locator", got)
		requireReceiptCondition(t, len(attempts) == 2, "unexpected attempts", attempts)
	}
	requireReceiptCondition(t, scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin`) == 0, "reads retained pins")
}
