//go:build linux

package receiptstore

import (
	"context"

	"github.com/cotocisternas/fry/internal/hotstore"
)

// dispatchMaintenance owns routing for explicit, blocking maintenance. A local
// incarnation is captured only under the exclusive gate; remote operations first
// refresh their authenticated identity. Each operation still owns its selection,
// capacity, publication and recovery policy. Automatic rotation uses its own
// nonblocking admission and identity adoption path.
func (s *Store) dispatchMaintenance(ctx context.Context, member ID, kind string) error {
	candidate := s.memberByID[member]
	if candidate == nil {
		return ErrNotFound
	}
	operation, err := hotstore.NewID()
	if err != nil {
		return err
	}
	if remote, ok := candidate.(*RemoteMember); ok {
		if err = remote.refresh(ctx); err != nil {
			return err
		}
		return remote.dispatchMaintenance(ctx, operation, kind)
	}
	m, ok := candidate.(*LocalMember)
	if !ok {
		return ErrConflict
	}
	return s.dispatchLocalMaintenance(ctx, m, operation, kind)
}

func (s *Store) dispatchLocalMaintenance(ctx context.Context, m *LocalMember, operation ID, kind string) error {
	m.gate.Lock()
	defer m.gate.Unlock()
	if m.transportOwned {
		return ErrStale
	}
	switch kind {
	case maintenanceKindRewrite:
		return s.reclaimMember(ctx, m, operation, nil, m.bytes.FileID())
	case maintenanceKindRotate:
		return s.rotateMember(ctx, m, operation, nil, m.bytes.FileID())
	case maintenanceKindSealedReclaim:
		return s.reclaimSealedMember(ctx, m, operation, nil, m.bytes.FileID())
	default:
		return ErrConflict
	}
}

func (m *RemoteMember) dispatchMaintenance(ctx context.Context, operation ID, kind string) error {
	switch kind {
	case maintenanceKindRewrite:
		return m.ReclaimHot(ctx, operation)
	case maintenanceKindRotate:
		return m.RotateHot(ctx, operation)
	case maintenanceKindSealedReclaim:
		return m.ReclaimSealed(ctx, operation)
	default:
		return ErrConflict
	}
}
