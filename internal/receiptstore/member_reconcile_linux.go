//go:build linux

package receiptstore

import (
	"context"
	"fmt"

	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

// ReconcileMemberClosures releases only obligations whose exact process
// generation has recorded actual storage closure. Lease expiry, loss of a
// connection, and a different live generation are never closure evidence.
// It does not fence a crashed process, erase bytes, or retire operation IDs.
// At most limit reads and limit aborted targets are reconciled per call.
func (s *Store) ReconcileMemberClosures(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, fmt.Errorf("receiptstore: invalid reconciliation batch size")
	}
	reads, err := s.reconcileClosedReads(ctx, limit)
	if err != nil {
		return 0, err
	}
	count := len(reads)
	targets, err := s.closedTargetCandidates(ctx, limit)
	if err != nil {
		return count, err
	}
	for _, item := range targets {
		changed, e := s.reconcileClosedTarget(ctx, item.attempt, item.member, item.epoch)
		if e != nil {
			return count, e
		}
		if changed {
			count++
		}
	}
	return count, nil
}

func (s *Store) reconcileClosedTarget(ctx context.Context, attempt, member string, epoch int64) (bool, error) {
	id, err := idFrom(attempt)
	if err != nil {
		return false, err
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(ctx, tx)
	record, err := attemptLock(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if record.state != "aborted" {
		return false, ErrStale
	}
	tag, err := dbgen.New(tx).
		StoreReconcileClosedTargetUpdateTarget(ctx, dbgen.StoreReconcileClosedTargetUpdateTargetParams{
			Attempt:    attempt,
			Member:     member,
			OwnerEpoch: epoch,
		})
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err = dbgen.New(tx).
		StoreReconcileClosedTargetUpdateCleanup(ctx, dbgen.StoreReconcileClosedTargetUpdateCleanupParams{
			Attempt: attempt,
			Member:  member,
		}); err != nil {
		return false, err
	}
	if err = commit(ctx, tx); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) reconcileClosedReads(ctx context.Context, limit int) ([]string, error) {
	tx, err := s.db.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	reads, err := closedReadCandidates(ctx, tx, limit)
	if err != nil {
		return nil, err
	}
	if len(reads) > 0 {
		if _, err = dbgen.New(tx).StoreReconcileMemberClosuresUpdateMemberRead(ctx, reads); err != nil {
			return nil, err
		}
		if _, err = dbgen.New(tx).StoreReconcileMemberClosuresDeleteReadPin(ctx, reads); err != nil {
			return nil, err
		}
	}
	if err = commit(ctx, tx); err != nil {
		return nil, err
	}
	return reads, nil
}

type closedTarget struct {
	attempt, member string
	epoch           int64
}

func (s *Store) closedTargetCandidates(ctx context.Context, limit int) ([]closedTarget, error) {
	// Select bounded candidates without retaining a cursor while opening their
	// transactions. Each attempt guard is acquired before touching its targets.
	rows, err := s.db.pool.Query(ctx, dbgen.StoreReconcileMemberClosuresReadTarget, limit)
	if err != nil {
		return nil, err
	}
	var targets []closedTarget
	for rows.Next() {
		var item closedTarget
		if err = rows.Scan(&item.attempt, &item.member, &item.epoch); err != nil {
			break
		}
		targets = append(targets, item)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	return targets, nil
}

func closedReadCandidates(ctx context.Context, tx pgx.Tx, limit int) ([]string, error) {
	rows, err := tx.Query(ctx, dbgen.StoreReconcileMemberClosuresReadMemberRead, limit)
	if err != nil {
		return nil, err
	}
	var reads []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		reads = append(reads, id)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	return reads, nil
}
