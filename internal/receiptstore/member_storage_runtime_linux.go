//go:build linux

package receiptstore

import (
	"context"
	"time"

	memberv1 "github.com/cotocisternas/fry/gen/member/go/fry/member/v1"
)

// Runtime is an observation, not a readiness or admission promise. Busy and
// unreachable samples leave guarded state unknown. No filesystem I/O is used.
type MemberStorageRuntime struct {
	Status           string     `json:"status"`
	CheckedAt        time.Time  `json:"checked_at"`
	SampledAt        *time.Time `json:"sampled_at"`
	Member           string     `json:"member"`
	FailureDomain    string     `json:"failure_domain"`
	BoundOwnerEpoch  *string    `json:"bound_owner_epoch"`
	OwnerEpoch       *string    `json:"owner_epoch"`
	CurrentFile      *string    `json:"current_file"`
	Closed           *bool      `json:"closed"`
	Frozen           *bool      `json:"frozen"`
	ClosureUncertain *bool      `json:"closure_uncertain"`
}

func (m *LocalMember) storageRuntime(epoch int64) MemberStorageRuntime {
	result := MemberStorageRuntime{
		Status:        "gate_busy",
		CheckedAt:     time.Now().UTC(),
		Member:        m.id.String(),
		FailureDomain: m.domain,
	}
	if epoch > 0 {
		value := storageDecimal(epoch)
		result.OwnerEpoch = &value
	}
	if !m.gate.TryRLock() {
		return result
	}
	now := time.Now().UTC()
	debt := m.readClosureUncertain.Load()
	closed, frozen := m.closed, m.frozen || debt
	file := m.bytes.FileID().String()
	if epoch == 0 && !m.transportOwned {
		value := "0"
		result.OwnerEpoch = &value
	}
	result.Status = "sampled"
	result.SampledAt = &now
	result.CurrentFile = &file
	result.Closed = &closed
	result.Frozen = &frozen
	result.ClosureUncertain = &debt
	m.gate.RUnlock()
	return result
}

func sampleMemberStorage(ctx context.Context, candidate Member) MemberStorageRuntime {
	if local, ok := candidate.(*LocalMember); ok {
		if ctx.Err() != nil {
			return MemberStorageRuntime{
				Status:        "unreachable",
				CheckedAt:     time.Now().UTC(),
				Member:        local.id.String(),
				FailureDomain: local.domain,
			}
		}
		return local.storageRuntime(0)
	}
	remote, ok := candidate.(*RemoteMember)
	if !ok {
		return MemberStorageRuntime{Status: "unreachable", CheckedAt: time.Now().UTC()}
	}
	return remote.storageRuntime(ctx)
}

func (m *RemoteMember) storageRuntime(ctx context.Context) MemberStorageRuntime {
	expected := m.identity()
	bound := storageDecimal(expected.OwnerEpoch)
	fallback := MemberStorageRuntime{
		Status:          "unreachable",
		CheckedAt:       time.Now().UTC(),
		Member:          expected.Member.String(),
		FailureDomain:   expected.FailureDomain,
		BoundOwnerEpoch: &bound,
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// Diagnostics never refresh or adopt the routing descriptor, nor wait on the
	// refresh mutex. This GET reports frozen owners without advertising identity.
	wire, err := m.rpc().StorageStatus(ctx, &memberv1.StorageStatusRequest{})
	if err != nil {
		return fallback
	}
	result, err := storageRuntimeFromProto(wire)
	if err != nil {
		fallback.Status = "invalid_response"
		return fallback
	}

	epoch, err := validateMemberRuntime(result, expected)
	if err != nil {
		fallback.Status = "invalid_response"
		return fallback
	}

	if epoch != expected.OwnerEpoch {
		fallback.Status = "generation_mismatch"
		fallback.OwnerEpoch = result.OwnerEpoch
		return fallback
	}
	result.CheckedAt = fallback.CheckedAt
	result.BoundOwnerEpoch = &bound
	return result
}
