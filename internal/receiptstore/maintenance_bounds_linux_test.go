//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pause at the real database boundary, before the failed-operation outcome takes
// its member lock. The test can then create actual PostgreSQL contention.
type maintenanceFinishBarrier struct {
	armed       atomic.Bool
	entered     chan context.Context
	release     chan struct{}
	testContext context.Context
}

func (barrier *maintenanceFinishBarrier) TraceQueryStart(
	ctx context.Context,
	_ *pgx.Conn,
	data pgx.TraceQueryStartData,
) context.Context {
	if strings.Contains(data.SQL, "-- name: LocalMemberFinishMaintenanceReadMember ") &&
		barrier.armed.CompareAndSwap(true, false) {
		barrier.entered <- ctx
		select {
		case <-barrier.release:
		case <-barrier.testContext.Done():
		}
	}
	return ctx
}
func (*maintenanceFinishBarrier) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestFailedMaintenanceOutcomeWaitIsBoundedAndRemainsFenced(t *testing.T) {
	for _, operation := range []maintenanceOperation{
		{"rotate", (*Store).RotateHot},
		{"auto-rotate", (*Store).RotateHotIfNeeded},
		{"sealed-reclaim", (*Store).ReclaimSealed},
		{"rewrite", (*Store).ReclaimHot},
	} {
		t.Run(operation.name, func(t *testing.T) {
			testFailedMaintenanceOutcome(t, operation)
		})
	}
}

type maintenanceOperation struct {
	name string
	run  func(*Store, context.Context, ID) error
}

func testFailedMaintenanceOutcome(t *testing.T, operation maintenanceOperation) {
	t.Helper()
	t.Parallel()
	barrier := &maintenanceFinishBarrier{
		entered:     make(chan context.Context, 1),
		release:     make(chan struct{}),
		testContext: t.Context(),
	}
	options := capacityOptions(2)
	options.MaxBytes += 4 * options.MaxFileBytes
	f := setupWithPoolConfig(
		t,
		func(config *pgxpool.Config) { config.ConnConfig.Tracer = barrier },
		options,
		Options{Lease: 10 * time.Second, RecoveryRetention: 24 * time.Hour},
	)
	dead := accept(t, f.s, newID(t), f.envelope(t), "expired original")
	live := accept(t, f.s, newID(t), f.envelope(t), "retained original")
	member := f.members[0]
	if operation.name == "sealed-reclaim" {
		requireReceiptNoError(t, f.s.RotateHot(t.Context(), member.id))
	}
	expireReceipt(t, f, dead)
	failure := errors.New("synthetic prepublication failure")
	cut := "after_register"
	if operation.name == "rewrite" {
		cut = "after_copy"
	}
	member.rewriteFault = func(point string) error {
		if point == cut {
			return failure
		}
		return nil
	}
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	barrier.armed.Store(true)
	go func() { done <- operation.run(f.s, parent, member.id) }()
	cleanup := awaitFailedMaintenanceCleanup(t, barrier, done, failure)
	locker := lockFailedMaintenanceOutcome(t, f, member, barrier, done, failure)
	defer rollback(t.Context(), locker)
	err := awaitFailedMaintenanceOutcome(t, t.Context(), cancel, cleanup, locker, done, failure)
	requireReceiptCondition(t, errors.Is(err, failure), "cleanup timeout lost failure or uncertainty", err)
	requireReceiptCondition(
		t,
		errors.Is(err, context.DeadlineExceeded),
		"cleanup timeout lost failure or uncertainty",
		err,
	)
	requireFailedMaintenanceReadAuthority(t, f, member, live)
}

func awaitFailedMaintenanceCleanup(
	t *testing.T,
	barrier *maintenanceFinishBarrier,
	done <-chan error,
	failure error,
) context.Context {
	t.Helper()
	var cleanup context.Context
	select {
	case cleanup = <-barrier.entered:
	case err := <-done:
		t.Fatal("operation never reached failed-outcome recording", err)
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not reach failed-outcome recording")
	}
	deadline, bounded := cleanup.Deadline()
	if !bounded {
		close(barrier.release)
		if err := <-done; !errors.Is(err, failure) {
			t.Error("original failure was lost", err)
		}
		t.Fatal("failed maintenance outcome has no cleanup deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 5*time.Second {
		t.Error("failed maintenance cleanup exceeds its five-second budget", remaining)
	}
	return cleanup
}

func lockFailedMaintenanceOutcome(
	t *testing.T,
	f *fixture,
	member *LocalMember,
	barrier *maintenanceFinishBarrier,
	done <-chan error,
	failure error,
) pgx.Tx {
	t.Helper()
	locker, err := f.pool.Begin(t.Context())
	if err != nil {
		close(barrier.release)
		if result := <-done; !errors.Is(result, failure) {
			t.Error("original failure was lost while joining maintenance", result)
		}
		t.Fatal(err)
	}
	returned := false
	defer func() {
		if !returned {
			rollback(t.Context(), locker)
		}
	}()
	if _, err = locker.Exec(
		t.Context(),
		`SELECT id FROM fry_member WHERE id=$1 FOR UPDATE`,
		member.id.String(),
	); err != nil {
		close(barrier.release)
		if result := <-done; !errors.Is(result, failure) {
			t.Error("original failure was lost while joining maintenance", result)
		}
		t.Fatal(err)
	}
	close(barrier.release)
	blocked, stop := context.WithTimeout(t.Context(), 2*time.Second)
	err = waitCapacityBlock(blocked, f, locker.Conn().PgConn().PID())
	stop()
	if err != nil {
		rollback(t.Context(), locker)
		if result := <-done; !errors.Is(result, failure) {
			t.Error("original failure was lost while joining maintenance", result)
		}
		t.Fatal("failed-outcome query did not wait on the actual member lock", err)
	}
	returned = true
	return locker
}

func awaitFailedMaintenanceOutcome(
	t *testing.T,
	request context.Context,
	cancel context.CancelFunc,
	cleanup context.Context,
	locker pgx.Tx,
	done <-chan error,
	failure error,
) error {
	t.Helper()
	var err error
	cancel()
	if cleanup.Err() != nil {
		t.Error("request cancellation canceled detached cleanup", cleanup.Err())
	}
	select {
	case err = <-done:
	case <-time.After(8 * time.Second):
		rollback(request, locker)
		if result := <-done; !errors.Is(result, failure) {
			t.Error("original failure was lost while joining maintenance", result)
		}
		t.Fatal("failed-outcome recording kept the exclusive gate indefinitely")
	}
	return err
}

func requireFailedMaintenanceReadAuthority(t *testing.T, f *fixture, member *LocalMember, live Receipt) {
	t.Helper()
	{
		err := f.s.RotateHotIfNeeded(t.Context(), member.id)
		requireReceiptCondition(
			t,
			errors.Is(err, ErrUncertain),
			"timed-out cleanup admitted work or retained the local gate",
			err,
		)
	}
	var output bytes.Buffer
	{
		err := f.s.Read(t.Context(), f.org, live.ID, 0, live.Size, &output)
		requireReceiptCondition(t, err == nil, "failed cleanup lost accepted content", err)
		requireReceiptCondition(t, output.String() == "retained original", "failed cleanup lost accepted content", err)
	}
}
