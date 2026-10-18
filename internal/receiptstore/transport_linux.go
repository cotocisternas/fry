//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"io"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

// Member is sealed to the local and authenticated remote adapters. Each adapter
// owns cancellation and storage completion; callers cannot substitute an adapter
// that acknowledges bytes without the corresponding durable authority checks.
type Member interface {
	identity() MemberIdentity
	available() bool
	write(context.Context, *authority, claim, io.ReadCloser) error
	cancelAttempt(context.Context, *authority, ID) error
	read(context.Context, ID, ID, ID, hotstore.Locator, int64, int64, io.Writer) error
}

func (m *LocalMember) identity() MemberIdentity {
	return MemberIdentity{Member: m.id, Incarnation: m.bytes.FileID(), FailureDomain: m.domain}
}
func (m *LocalMember) available() bool { return !m.closed && !m.isFrozen() && !m.transportOwned }

func (m *LocalMember) read(
	ctx context.Context,
	pin, org, receipt ID,
	loc hotstore.Locator,
	offset, length int64,
	dst io.Writer,
) error {
	if m.transportOwned {
		return ErrStale
	}
	if m.closed || m.isFrozen() {
		return ErrStale
	}
	release, err := m.acquireRead(ctx)
	if err != nil {
		return err
	}
	defer release()
	source, err := resolveMemberRead(ctx, m.pool, m, pin, org, receipt)
	if err != nil {
		return err
	}
	return m.readSource(ctx, source, offset, length, dst)
}

// guardPlacement serializes target registration with member replacement. A
// remote descriptor is obtained over authenticated TLS and then checked against
// the same database authority used by the receiving member.
func guardPlacement(ctx context.Context, tx pgx.Tx, id MemberIdentity) (memberCapacityProfile, error) {
	row, err := dbgen.New(tx).GuardPlacementReadMember(ctx, id.Member.String())
	if err != nil {
		return memberCapacityProfile{}, err
	}
	limits := memberCapacityProfile{
		enabled:    row.Enabled,
		maxMsg:     row.MaxMessageBytes,
		maxRecords: row.MaxRecords,
		maxFile:    row.MaxFileBytes,
		lanes:      int64(row.LaneCount),
	}
	if row.MaintenanceJob != nil {
		return limits, ErrBusy
	}
	if row.Incarnation != id.Incarnation.String() || row.FailureDomain != id.FailureDomain {
		return limits, ErrStale
	}
	return limits, guardPlacementOwner(ctx, tx, id)
}

func guardPlacementOwner(ctx context.Context, tx pgx.Tx, id MemberIdentity) error {
	row, err := dbgen.New(tx).GuardPlacementReadMemberOwner(ctx, id.Member.String())
	if errors.Is(err, pgx.ErrNoRows) && id.OwnerEpoch == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	if id.OwnerEpoch == 0 {
		if !row.Closed {
			return ErrStale
		}
		return nil
	}
	if row.Epoch != id.OwnerEpoch || row.Closed {
		return ErrStale
	}
	return nil
}
