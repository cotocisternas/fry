//go:build linux

package receiptstore

import (
	"context"

	memberv1 "github.com/cotocisternas/fry/gen/member/go/fry/member/v1"
	"github.com/cotocisternas/fry/internal/hotstore"
)

func (m *RemoteMember) maintenance(
	ctx context.Context,
	operation ID,
	kind memberv1.MaintenanceKind,
	status bool,
) (*memberv1.MaintenanceResponse, error) {
	result, err := m.rpc().
		Maintenance(ctx, &memberv1.MaintenanceRequest{
			Target:     m.target(claim{}),
			Operation:  operation.String(),
			Kind:       kind,
			StatusOnly: status,
		})
	return result, remoteRPCError(err)
}

func (m *RemoteMember) runMaintenance(
	ctx context.Context,
	operation ID,
	kind memberv1.MaintenanceKind,
	refresh bool,
) error {
	result, err := m.maintenance(ctx, operation, kind, false)
	if err != nil {
		return err
	}
	if result.State != maintenanceComplete || result.Identity != nil {
		return ErrConflict
	}
	if refresh {
		return m.refresh(ctx)
	}
	return nil
}

func (m *RemoteMember) maintenanceStatus(
	ctx context.Context,
	operation ID,
	kind memberv1.MaintenanceKind,
) (string, error) {
	result, err := m.maintenance(ctx, operation, kind, true)
	if err != nil {
		return "", err
	}
	if result.Identity != nil {
		return "", ErrConflict
	}
	switch result.State {
	case maintenanceRunning, maintenanceComplete, maintenanceFailed:
		return result.State, nil
	default:
		return "", ErrConflict
	}
}

func (m *RemoteMember) ReclaimHot(ctx context.Context, operation ID) error {
	return m.runMaintenance(ctx, operation, memberv1.MaintenanceKind_MAINTENANCE_KIND_REWRITE, true)
}

func (m *RemoteMember) RotateHot(ctx context.Context, operation ID) error {
	return m.runMaintenance(ctx, operation, memberv1.MaintenanceKind_MAINTENANCE_KIND_ROTATE, true)
}

func (m *RemoteMember) ReclaimSealed(ctx context.Context, operation ID) error {
	return m.runMaintenance(ctx, operation, memberv1.MaintenanceKind_MAINTENANCE_KIND_SEALED_RECLAIM, false)
}

func (m *RemoteMember) MaintenanceStatus(ctx context.Context, operation ID) (string, error) {
	return m.maintenanceStatus(ctx, operation, memberv1.MaintenanceKind_MAINTENANCE_KIND_REWRITE)
}

func (m *RemoteMember) RotationStatus(ctx context.Context, operation ID) (string, error) {
	return m.maintenanceStatus(ctx, operation, memberv1.MaintenanceKind_MAINTENANCE_KIND_ROTATE)
}

func (m *RemoteMember) SealedReclaimStatus(ctx context.Context, operation ID) (string, error) {
	return m.maintenanceStatus(ctx, operation, memberv1.MaintenanceKind_MAINTENANCE_KIND_SEALED_RECLAIM)
}

func (m *RemoteMember) AutomaticRotationStatus(ctx context.Context, operation ID) (string, error) {
	return m.maintenanceStatus(ctx, operation, memberv1.MaintenanceKind_MAINTENANCE_KIND_AUTO_ROTATE)
}

func (m *RemoteMember) RotateHotIfNeeded(ctx context.Context, operation ID) error {
	if !m.refreshMu.TryLock() {
		return ErrBusy
	}
	defer m.refreshMu.Unlock()
	result, err := m.maintenance(ctx, operation, memberv1.MaintenanceKind_MAINTENANCE_KIND_AUTO_ROTATE, false)
	if err != nil {
		return err
	}
	return m.adoptAutomaticResult(result)
}

func (m *RemoteMember) adoptAutomaticResult(result *memberv1.MaintenanceResponse) error {
	if result.State == maintenanceComplete || result.State == "stale" {
		return m.adoptAutomaticIdentity(result)
	}

	if result.Identity != nil {
		return ErrConflict
	}
	switch result.State {
	case "conflict":
		return ErrConflict
	case "busy":
		return ErrBusy
	case "capacity":
		return hotstore.ErrCapacity
	case "uncertain":
		return ErrUncertain
	default:
		return ErrConflict
	}
}

func (m *RemoteMember) adoptAutomaticIdentity(result *memberv1.MaintenanceResponse) error {
	current, err := identityFromProto(result.Identity)
	if err != nil {
		return err
	}
	if err = m.adopt(current); err != nil {
		return err
	}
	if result.State == "stale" {
		return ErrStale
	}
	return nil
}
