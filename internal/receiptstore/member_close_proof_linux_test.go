//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
)

func requireMemberRootOwnership(t *testing.T, m *LocalMember) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(m.root, "member.lock"), os.O_RDWR, 0)
	requireReceiptNoError(t, err)
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
			t.Error(err)
		}
		t.Fatal("member released root ownership without byte-close proof")
	}
	requireReceiptCondition(t, errors.Is(err, syscall.EWOULDBLOCK), "root ownership was not independently proven", err)
}

// Only the test may clear this simulated debt: the real store.Close succeeded,
// all test operations have joined, and no descriptor's actual closure is unknown.
func cleanupKnownMemberClose(t *testing.T, m *LocalMember) {
	t.Helper()
	m.storeClose = nil
	if err := m.bytes.Close(); err != nil {
		t.Error("actual store cleanup failed", err)
		return
	}
	m.readClosureUncertain.Store(false)
	m.frozen = false
	if err := m.Close(); err != nil {
		t.Error("proven member root cleanup failed", err)
	}
}

func TestMemberCloseLostByteProofRetainsRootOnRepeatedClose(t *testing.T) {
	f := capacityFixture(t, 2)
	m := f.members[0]
	accept(t, f.s, newID(t), f.envelope(t), "accepted original survives lost close proof")
	requireReceiptNoError(
		t,
		os.WriteFile(filepath.Join(m.root, "stage-terminal-close-sentinel"), []byte("do not recover staging"), 0o600),
	)
	before := memberFilesTree(t, m.root)
	proofLost := errors.New("synthetic lost current-store close proof")
	// The actual writer and descriptor close successfully. Returning an error
	// afterwards simulates proof loss, never a real operating-system close fault.
	calls, actuallyClosed := injectOwnedStoreClosureLoss(t, m, proofLost)
	defer cleanupKnownMemberClose(t, m)
	first := m.Close()
	requireReceiptCondition(t, actuallyClosed.Load(), "byte-close proof loss not exercised or classified", first)
	requireReceiptCondition(
		t,
		errors.Is(first, ErrUncertain),
		"byte-close proof loss not exercised or classified",
		first,
	)
	requireReceiptCondition(t, errors.Is(first, proofLost), "byte-close proof loss not exercised or classified", first)
	for range 2 {
		{
			err := m.Close()
			requireReceiptCondition(
				t,
				errors.Is(err, ErrUncertain),
				"repeated Close fabricated clean storage proof",
				err,
			)
		}
	}
	requireReceiptCondition(
		t,
		calls.Load() == 1,
		"repeated terminal Close retried an unproven byte close",
		calls.Load(),
	)
	m.gate.RLock()
	frozen, available := m.isFrozen(), m.available()
	m.gate.RUnlock()
	requireReceiptCondition(t, frozen, "terminal close uncertainty did not freeze member admission")
	requireReceiptCondition(t, !available, "terminal close uncertainty did not freeze member admission")
	requireMemberRootOwnership(t, m)
	next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, m.opts)
	if err == nil {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("second open reused an unproven member root")
	}
	requireReceiptCondition(
		t,
		errors.Is(err, hotstore.ErrBusy),
		"second open failed without proving retained root ownership",
		err,
	)
	memberFilesUnchanged(t, m.root, before)
}

func TestMemberOwnerCloseLostByteProofKeepsGenerationUnclosed(t *testing.T) {
	f := capacityFixture(t, 2)
	m := f.members[0]
	accept(t, f.s, newID(t), f.envelope(t), "owned accepted original")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner, err := ownMember(ctx, m)
	requireReceiptNoError(t, err)
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if e := owner.Close(cleanup); e != nil && !errors.Is(e, ErrUncertain) {
			t.Error("owner fixture cleanup failed", e)
		}
	}()
	err = os.WriteFile(
		filepath.Join(m.root, "stage-owner-close-sentinel"),
		[]byte("preserve across refused restart"),
		0o600,
	)
	requireReceiptNoError(t, err)
	before := memberFilesTree(t, m.root)
	proofLost := errors.New("synthetic lost owner current-store close proof")
	calls, actuallyClosed := injectOwnedStoreClosureLoss(t, m, proofLost)
	defer cleanupKnownMemberClose(t, m)
	for range 2 {
		err = owner.Close(ctx)
		requireReceiptCondition(t, errors.Is(err, ErrUncertain), "owner Close fabricated clean generation proof", err)
		requireReceiptCondition(t, errors.Is(err, proofLost), "owner Close fabricated clean generation proof", err)
	}
	requireReceiptCondition(
		t,
		actuallyClosed.Load(),
		"real owned store close was not exercised exactly once",
		calls.Load(),
	)
	requireReceiptCondition(t, calls.Load() == 1, "real owned store close was not exercised exactly once", calls.Load())
	requireUncertainOwnerRetainsRoot(t, f, m, owner)
	memberFilesUnchanged(t, m.root, before)
	requireUnclosedGenerationFencesRestart(t, f, m, before)
}

func requireUnclosedGenerationFencesRestart(t *testing.T, f *fixture, m *LocalMember, before map[string]string) {
	t.Helper()
	// Release only the test's proven-closed root. The durable unclosed generation
	// must independently fence restart after the advisory connection has ended.
	cleanupKnownMemberClose(t, m)
	next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, m.opts)
	if err == nil {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("closed DB session was mistaken for owner-generation closure")
	}
	requireReceiptCondition(t, errors.Is(err, ErrBusy), "unclosed generation did not fence restart", err)
	memberFilesUnchanged(t, m.root, before)
}

func injectOwnedStoreClosureLoss(t *testing.T, m *LocalMember, proofLost error) (*atomic.Int32, *atomic.Bool) {
	t.Helper()
	var calls atomic.Int32
	var actuallyClosed atomic.Bool
	m.storeClose = func(store *hotstore.Store) error {
		calls.Add(1)
		if err := store.Close(); err != nil {
			return err
		}
		actuallyClosed.Store(true)
		return proofLost
	}
	return &calls, &actuallyClosed
}

func requireUncertainOwnerRetainsRoot(t *testing.T, f *fixture, m *LocalMember, owner *memberOwner) {
	t.Helper()
	var err error
	requireReceiptCondition(t, owner.connection.IsClosed(), "owner database session did not drain and close")
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_owner WHERE member=$1 AND epoch=$2 AND instance=$3 `+
				`AND NOT closed`,
			m.id.String(),
			owner.epoch,
			owner.instance.String(),
		) == 1,
		"drained owner published false clean closure",
	)
	{
		err = m.Close()
		requireReceiptCondition(
			t,
			errors.Is(err, ErrUncertain),
			"member shortcut discarded owner close uncertainty",
			err,
		)
	}
	requireReceiptCondition(t, m.isFrozen(), "owned terminal close proof loss did not freeze member")
	requireMemberRootOwnership(t, m)
	if next, e := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, m.opts); e == nil {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("uncertain owned root reopened")
	}
}
