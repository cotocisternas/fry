//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"time"

	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

// These operation kinds identify durable maintenance retries, including the
// distinction between an explicit rotation and automatic capacity maintenance.
const (
	maintenanceKindRewrite       = "rewrite"
	maintenanceKindRotate        = "rotate"
	maintenanceKindSealedReclaim = "sealed-reclaim"
	maintenanceKindAutoRotate    = "auto-rotate"
)

// Durable job states are also returned by the member maintenance status RPC.
// Automatic rotation's admission outcomes (busy, stale, etc.) are separate.
const (
	maintenanceRunning  = "running"
	maintenanceComplete = "complete"
	maintenanceFailed   = "failed"
)

// beginMaintenance holds only the member row, before taking attempt/receipt
// locks. Committing this gate excludes remotely prepared targets as well as I/O
// drained by the local gate. Timeouts never clear a committed gate.
func (m *LocalMember) beginMaintenance(
	ctx context.Context,
	operation ID,
	owner *memberOwner,
	expected ID,
) (bool, error) {
	return m.beginMaintenanceKind(ctx, operation, owner, expected, maintenanceKindRewrite)
}

func (m *LocalMember) beginMaintenanceKind(
	ctx context.Context,
	operation ID,
	owner *memberOwner,
	expected ID,
	kind string,
) (bool, error) {
	if operation == (ID{}) || !validMaintenanceKind(kind) {
		return false, ErrConflict
	}
	tx, err := (&authority{pool: m.pool}).begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(ctx, tx)
	binding, err := dbgen.New(tx).LocalMemberBeginMaintenanceKindReadMember(ctx, m.id.String())
	if err != nil {
		return false, err
	}
	epoch := int64(0)
	if owner != nil {
		epoch = owner.epoch
		if err = owner.guard(ctx, tx); err != nil {
			return false, err
		}
	}
	stored, err := dbgen.New(tx).LocalMemberBeginMaintenanceKindReadMemberMaintenance(ctx, operation.String())
	if err == nil {
		return m.checkMaintenanceRetry(stored, epoch, kind, operation, binding.MaintenanceJob, binding.Incarnation)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	return false, m.startMaintenance(ctx, tx, operation, epoch, expected, kind, binding)
}

func validMaintenanceKind(kind string) bool {
	return kind == maintenanceKindRewrite || kind == maintenanceKindRotate ||
		kind == maintenanceKindSealedReclaim || kind == maintenanceKindAutoRotate
}

func (m *LocalMember) checkMaintenanceRetry(
	stored dbgen.LocalMemberBeginMaintenanceKindReadMemberMaintenanceRow,
	epoch int64,
	kind string,
	operation ID,
	active *string,
	incarnation string,
) (bool, error) {
	if stored.Member != m.id.String() || stored.OwnerEpoch != epoch || stored.Kind != kind {
		return false, ErrConflict
	}
	if stored.State == maintenanceComplete {
		return true, nil
	}
	// Only an exact operation retry may pass a changed physical incarnation.
	if stored.State != maintenanceRunning || active == nil || *active != operation.String() {
		return false, ErrStale
	}
	if stored.ExpectedIncarnation != incarnation {
		return false, ErrUncertain
	}
	return false, nil
}

func (m *LocalMember) startMaintenance(
	ctx context.Context,
	tx pgx.Tx,
	operation ID,
	epoch int64,
	expected ID,
	kind string,
	binding dbgen.LocalMemberBeginMaintenanceKindReadMemberRow,
) error {
	if binding.MaintenanceJob != nil {
		return ErrBusy
	}
	if binding.Incarnation != expected.String() || binding.Incarnation != m.bytes.FileID().String() {
		return ErrStale
	}
	if _, err := dbgen.New(tx).
		LocalMemberBeginMaintenanceKindInsertMemberMaintenance(
			ctx,
			dbgen.LocalMemberBeginMaintenanceKindInsertMemberMaintenanceParams{
				ID:                  operation.String(),
				Member:              m.id.String(),
				OwnerEpoch:          epoch,
				ExpectedIncarnation: binding.Incarnation,
				Kind:                kind,
			},
		); err != nil {
		return err
	}
	if _, err := dbgen.New(tx).
		LocalMemberBeginMaintenanceKindUpdateMember(ctx, dbgen.LocalMemberBeginMaintenanceKindUpdateMemberParams{
			ID:             m.id.String(),
			MaintenanceJob: sqlcPointer(operation.String()),
		}); err != nil {
		return err
	}
	if err := commit(ctx, tx); err != nil {
		m.frozen = true
		return err
	}
	return nil
}

// failMaintenance runs only after unpublished work was reconciled. Request
// cancellation must not strand its outcome, but a stalled database must not hold
// the exclusive local gate indefinitely. Failure leaves the caller frozen.
func (m *LocalMember) failMaintenance(ctx context.Context, operation ID) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return m.finishMaintenance(cleanup, operation, maintenanceFailed)
}

func (m *LocalMember) finishMaintenance(ctx context.Context, operation ID, state string) error {
	tx, err := (&authority{pool: m.pool}).begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	var active *string
	if err = func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).LocalMemberFinishMaintenanceReadMember(ctx, m.id.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		active = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	if active == nil || *active != operation.String() {
		return ErrStale
	}
	if _, err = dbgen.New(tx).
		LocalMemberFinishMaintenanceUpdateMemberMaintenance(
			ctx,
			dbgen.LocalMemberFinishMaintenanceUpdateMemberMaintenanceParams{
				ID:    operation.String(),
				State: state,
			},
		); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).LocalMemberFinishMaintenanceUpdateMember(ctx, m.id.String()); err != nil {
		return err
	}
	return commit(ctx, tx)
}

// Called only after OpenLocalMember proved previous owners closed and recovered
// the database-selected generation plus every tracked rewrite directory.
func (m *LocalMember) recoverMaintenance(ctx context.Context) error {
	tx, err := (&authority{pool: m.pool}).begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	var active *string
	if err = func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).LocalMemberRecoverMaintenanceReadMember(ctx, m.id.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		active = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	if active == nil {
		return nil
	}
	if _, err = dbgen.New(tx).LocalMemberRecoverMaintenanceUpdateMemberMaintenance(ctx, *active); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).LocalMemberRecoverMaintenanceUpdateMember(ctx, m.id.String()); err != nil {
		return err
	}
	return commit(ctx, tx)
}
