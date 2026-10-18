//go:build linux

package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

type quotaEvaluatorFunc func(context.Context, int) (int, error)

func (f quotaEvaluatorFunc) EvaluateStorageQuotas(ctx context.Context, limit int) (int, error) {
	return f(ctx, limit)
}

func TestStorageQuotaWorkerDrainsFullBatchAndCancelsInFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan struct{})
	calls := 0
	evaluator := quotaEvaluatorFunc(func(call context.Context, limit int) (int, error) {
		calls++
		if limit != 64 {
			t.Errorf("batch limit = %d", limit)
		}
		if _, ok := call.Deadline(); !ok {
			t.Error("evaluation has no deadline")
		}
		if calls == 1 {
			return 64, nil
		}
		close(entered)
		<-call.Done()
		return 0, call.Err()
	})
	go func() { defer close(done); runStorageQuotaEvaluator(ctx, evaluator, time.Hour) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("full batch did not continue before the next tick")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight evaluation did not stop")
	}
}

func TestStorageQuotaWorkerWaitsAfterPartialFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	reentered := make(chan struct{}, 1)
	done := make(chan struct{})
	evaluator := quotaEvaluatorFunc(func(context.Context, int) (int, error) {
		recordQuotaEntry(entered, reentered)
		return 64, errors.New("synthetic evaluation failure")
	})
	go func() { defer close(done); runStorageQuotaEvaluator(ctx, evaluator, time.Hour) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not attempt evaluation")
	}
	select {
	case <-reentered:
		t.Error("partial failed batch was immediately retried")
	case <-time.After(50 * time.Millisecond):
	}
	// Cancellation must interrupt the wait, rather than retrying a failed batch.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop while awaiting its next tick")
	}
}

func recordQuotaEntry(entered chan struct{}, reentered chan struct{}) {
	select {
	case <-entered:
		select {
		case reentered <- struct{}{}:
		default:
		}
	default:
		close(entered)
	}
}
