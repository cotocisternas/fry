//go:build linux

package receiptstore

import (
	"context"
	"errors"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

// RotateHotIfNeeded never waits for an active local read or write to drain. It
// considers only bounded files whose durable lanes cannot admit another maximum
// sized record. Admission never invokes this operation or retries through it.
func (s *Store) RotateHotIfNeeded(ctx context.Context, member ID) error {
	candidate := s.memberByID[member]
	if candidate == nil {
		return ErrNotFound
	}
	operation, err := hotstore.NewID()
	if err != nil {
		return err
	}
	if remote, ok := candidate.(*RemoteMember); ok {
		return remote.RotateHotIfNeeded(ctx, operation)
	}
	m, ok := candidate.(*LocalMember)
	if !ok {
		return ErrConflict
	}
	if !m.gate.TryLock() {
		return ErrBusy
	}
	defer m.gate.Unlock()
	if m.transportOwned {
		return ErrStale
	}
	return s.rotateIfNeededMember(ctx, m, operation, nil, m.bytes.FileID())
}

func (s *Store) rotateIfNeededMember(
	ctx context.Context,
	m *LocalMember,
	operation ID,
	owner *memberOwner,
	expected ID,
) error {
	if m.closed {
		return hotstore.ErrClosed
	}
	if m.isFrozen() {
		return ErrUncertain
	}
	complete, err := m.beginMaintenanceKind(ctx, operation, owner, expected, maintenanceKindAutoRotate)
	if err != nil || complete {
		return err
	}
	needed, err := s.automaticRotationNeeded(ctx, m)
	if err == nil && needed {
		err = s.rotateHot(ctx, m, operation, owner)
	}
	if err != nil {
		return m.recoverAutomaticRotation(ctx, operation, err)
	}
	if err = m.finishMaintenance(ctx, operation, maintenanceComplete); err != nil {
		m.frozen = true
	}
	return err
}

func (s *Store) automaticRotationNeeded(ctx context.Context, m *LocalMember) (bool, error) {
	if m.opts.MaxRecords == 0 {
		return false, nil
	}
	encoded, err := hotstore.EncodedRecordBytes(m.opts.MaxMessageBytes)
	if err != nil {
		return false, err
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(ctx, tx)
	var lanes int64
	var fits bool
	if err = func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			StoreAutomaticRotationNeeded(ctx, dbgen.StoreAutomaticRotationNeededParams{
				Member:  m.id.String(),
				FileID:  m.bytes.FileID().String(),
				Column3: encoded,
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		lanes = sqlcResult.Count
		fits = sqlcResult.Column2
		return nil
	}(); err != nil {
		return false, err
	}
	if lanes != memberCapacityLaneCount(m.opts) {
		return false, ErrConflict
	}
	if fits {
		return false, nil
	}
	return s.rotationHasRecords(ctx, tx, m)
}

func (s *Store) rotationHasRecords(ctx context.Context, tx pgx.Tx, m *LocalMember) (bool, error) {
	// Empty physical files can still have unresolved reservations. Only a closed
	// target census proves this no-op cannot hide pending or uncertain writes.
	image, err := m.bytes.Snapshot()
	if err != nil {
		return false, err
	}
	size := image.Size()
	if err = image.Close(); err != nil {
		return false, rotationClosureError(m, err)
	}
	if size == memberFileHeaderBytes {
		if _, err = s.snapshotHot(ctx, tx, m); err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

// Recover only after the rotation attempt has returned. Uncertain outcomes stay
// fenced; confirmed failures reconcile rewrites before releasing maintenance.
func (m *LocalMember) recoverAutomaticRotation(ctx context.Context, operation ID, err error) error {
	if errors.Is(err, ErrUncertain) || errors.Is(err, hotstore.ErrUncertain) {
		m.frozen = true
	}
	if m.isFrozen() {
		return err
	}
	if recoveryErr := m.reconcileRewrites(ctx); recoveryErr != nil {
		m.frozen = true
		return errors.Join(err, recoveryErr)
	}
	if finishErr := m.failMaintenance(ctx, operation); finishErr != nil {
		m.frozen = true
		return errors.Join(err, finishErr)
	}
	return err
}
