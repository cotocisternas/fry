//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	memberv1 "github.com/cotocisternas/fry/gen/member/go/fry/member/v1"
)

func maintenanceKind(kind memberv1.MaintenanceKind) string {
	switch kind {
	case memberv1.MaintenanceKind_MAINTENANCE_KIND_REWRITE:
		return maintenanceKindRewrite
	case memberv1.MaintenanceKind_MAINTENANCE_KIND_ROTATE:
		return maintenanceKindRotate
	case memberv1.MaintenanceKind_MAINTENANCE_KIND_SEALED_RECLAIM:
		return maintenanceKindSealedReclaim
	case memberv1.MaintenanceKind_MAINTENANCE_KIND_AUTO_ROTATE:
		return maintenanceKindAutoRotate
	case memberv1.MaintenanceKind_MAINTENANCE_KIND_UNSPECIFIED:
		return ""
	default:
		return ""
	}
}

func (rpc *memberRPC) Maintenance(
	ctx context.Context,
	req *memberv1.MaintenanceRequest,
) (*memberv1.MaintenanceResponse, error) {
	if err := rpc.checkTarget(req.Target, false); err != nil {
		return nil, err
	}
	operation, err := idFrom(req.Operation)
	if err != nil || operation == (ID{}) || maintenanceKind(req.Kind) == "" {
		return nil, ErrConflict
	}
	if req.StatusOnly {
		return rpc.maintenanceStatus(ctx, req, operation)
	}
	m := rpc.server.owner.member
	if !rpc.lockMaintenance(req.Kind) {
		return &memberv1.MaintenanceResponse{State: "busy"}, nil
	}

	defer m.gate.Unlock()
	incarnation, err := idFrom(req.Target.Incarnation)
	if err != nil {
		return nil, err
	}
	err = rpc.runMaintenance(ctx, req.Kind, operation, incarnation)
	if req.Kind == memberv1.MaintenanceKind_MAINTENANCE_KIND_AUTO_ROTATE {
		return rpc.automaticResult(err), nil
	}
	if err != nil {
		return nil, err
	}
	return &memberv1.MaintenanceResponse{State: maintenanceComplete}, nil
}

func (rpc *memberRPC) lockMaintenance(kind memberv1.MaintenanceKind) bool {
	m := rpc.server.owner.member
	if kind == memberv1.MaintenanceKind_MAINTENANCE_KIND_AUTO_ROTATE {
		return m.gate.TryLock()
	}
	m.gate.Lock()
	return true
}

func (rpc *memberRPC) runMaintenance(
	ctx context.Context,
	kind memberv1.MaintenanceKind,
	operation, incarnation ID,
) error {
	m := rpc.server.owner.member
	store := &Store{db: &authority{pool: m.pool, opts: Options{}}}
	switch kind {
	case memberv1.MaintenanceKind_MAINTENANCE_KIND_REWRITE:
		return store.reclaimMember(ctx, m, operation, rpc.server.owner, incarnation)
	case memberv1.MaintenanceKind_MAINTENANCE_KIND_ROTATE:
		return store.rotateMember(ctx, m, operation, rpc.server.owner, incarnation)
	case memberv1.MaintenanceKind_MAINTENANCE_KIND_SEALED_RECLAIM:
		return store.reclaimSealedMember(ctx, m, operation, rpc.server.owner, incarnation)
	case memberv1.MaintenanceKind_MAINTENANCE_KIND_AUTO_ROTATE:
		return store.rotateIfNeededMember(ctx, m, operation, rpc.server.owner, incarnation)
	case memberv1.MaintenanceKind_MAINTENANCE_KIND_UNSPECIFIED:
		return ErrConflict
	default:
		return ErrConflict
	}
}

func (rpc *memberRPC) maintenanceStatus(
	ctx context.Context,
	req *memberv1.MaintenanceRequest,
	operation ID,
) (*memberv1.MaintenanceResponse, error) {
	m := rpc.server.owner.member
	result, err := dbgen.New(m.pool).
		MemberServerServeReadMemberMaintenance(ctx, dbgen.MemberServerServeReadMemberMaintenanceParams{
			ID:     operation.String(),
			Member: m.id.String(),
		})
	if err != nil {
		return nil, err
	}
	if result.Kind != maintenanceKind(req.Kind) || result.OwnerEpoch != req.Target.OwnerEpoch {
		return nil, ErrConflict
	}
	return &memberv1.MaintenanceResponse{State: result.State}, nil
}

func automaticState(err error) string {
	switch {
	case err == nil:
		return maintenanceComplete
	case errors.Is(err, ErrStale):
		return "stale"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrBusy):
		return "busy"
	case errors.Is(err, hotstore.ErrCapacity):
		return "capacity"
	default:
		return "uncertain"
	}
}

func (rpc *memberRPC) automaticResult(err error) *memberv1.MaintenanceResponse {
	result := &memberv1.MaintenanceResponse{State: automaticState(err)}
	m := rpc.server.owner.member
	if (result.State == maintenanceComplete || result.State == "stale") && !m.closed && !m.isFrozen() {
		result.Identity = identityProto(
			MemberIdentity{
				Member:        m.id,
				Incarnation:   m.bytes.FileID(),
				FailureDomain: m.domain,
				OwnerEpoch:    rpc.server.owner.epoch,
			},
		)
	}
	return result
}

func (rpc *memberRPC) StorageStatus(
	context.Context,
	*memberv1.StorageStatusRequest,
) (*memberv1.StorageStatusResponse, error) {
	result := rpc.server.owner.member.storageRuntime(rpc.server.owner.epoch)
	wire := &memberv1.StorageStatusResponse{
		Status:           result.Status,
		CheckedAt:        result.CheckedAt.Format(time.RFC3339Nano),
		Member:           result.Member,
		FailureDomain:    result.FailureDomain,
		OwnerEpoch:       result.OwnerEpoch,
		CurrentFile:      result.CurrentFile,
		Closed:           result.Closed,
		Frozen:           result.Frozen,
		ClosureUncertain: result.ClosureUncertain,
	}
	if result.SampledAt != nil {
		value := result.SampledAt.Format(time.RFC3339Nano)
		wire.SampledAt = &value
	}
	return wire, nil
}
