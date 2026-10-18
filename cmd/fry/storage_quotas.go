//go:build linux

package main

import (
	"context"
	"log"
	"time"
)

type storageQuotaEvaluator interface {
	EvaluateStorageQuotas(context.Context, int) (int, error)
}

// Quotas are advisory. This worker records sampled alerts and never gates mail
// receipt. A full batch continues immediately; due-policy ordering belongs to
// the evaluator so organizations do not starve behind repeatedly sampled rows.
func startStorageQuotaEvaluator(ctx context.Context, evaluator storageQuotaEvaluator) func() {
	work, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runStorageQuotaEvaluator(work, evaluator, time.Second)
	}()
	return func() { cancel(); <-done }
}

func runStorageQuotaEvaluator(ctx context.Context, evaluator storageQuotaEvaluator, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failed := false
	for ctx.Err() == nil {
		call, cancel := context.WithTimeout(ctx, 10*time.Second)
		n, err := evaluator.EvaluateStorageQuotas(call, 64)
		cancel()
		if ctx.Err() != nil {
			return
		}
		reportQuotaEvaluation(err, failed)
		failed = err != nil
		if err == nil && n == 64 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func reportQuotaEvaluation(err error, failed bool) {
	if err != nil && !failed {
		log.Print("Storage quota alert evaluation will retry; stored samples may be stale")
	} else if err == nil && failed {
		log.Print("Storage quota alert evaluation recovered")
	}
}
