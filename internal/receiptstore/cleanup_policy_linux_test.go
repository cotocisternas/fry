//go:build linux

package receiptstore

import (
	"context"
	"testing"
	"time"
)

func TestCleanupDeadlineUsesAcceptedPolicyAcrossReclaimers(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	const acceptedDeadline = 2 * time.Hour
	f.s.db.opts.CleanupDeadline = acceptedDeadline
	receipt := accept(t, f.s, newID(t), f.envelope(t), "accepted under the original cleanup policy")
	if got := scalar(
		t,
		f.pool,
		`SELECT cleanup_microseconds FROM fry_attempt WHERE id=$1`,
		receipt.Attempt.String(),
	); got != acceptedDeadline.Microseconds() {
		t.Fatalf("attempt captured cleanup deadline %d, want %d", got, acceptedDeadline.Microseconds())
	}
	if _, err := f.pool.Exec(
		ctx,
		`UPDATE fry_reference SET expires_at=clock_timestamp()-interval '1 hour' WHERE `+
			`receipt=$1`,
		receipt.ID.String(),
	); err != nil {
		t.Fatal(err)
	}
	// A replica owner can reconcile lifetime with no configured Store deadline.
	// Its local settings must not replace the policy captured by acceptance.
	reclaimer := &Store{db: &authority{pool: f.pool}}
	if n, err := reclaimer.SweepExpiry(ctx, 1); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	status, err := reclaimer.CleanupStatus(ctx, f.org, receipt.ID)
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, status.State == "retiring" && status.EligibleAt != nil && status.Due != nil, status)
	if got := status.Due.Sub(*status.EligibleAt); got != acceptedDeadline {
		t.Fatalf("cleanup deadline changed with reclaimer settings: got %s, want %s", got, acceptedDeadline)
	}
}
