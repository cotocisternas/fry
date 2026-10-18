//go:build linux

package receiptstore

import (
	"strconv"
	"time"

	memberv1 "github.com/cotocisternas/fry/gen/member/go/fry/member/v1"
)

func storageRuntimeFromProto(wire *memberv1.StorageStatusResponse) (MemberStorageRuntime, error) {
	if wire == nil {
		return MemberStorageRuntime{}, ErrConflict
	}
	result := MemberStorageRuntime{
		Status:           wire.Status,
		Member:           wire.Member,
		FailureDomain:    wire.FailureDomain,
		OwnerEpoch:       wire.OwnerEpoch,
		CurrentFile:      wire.CurrentFile,
		Closed:           wire.Closed,
		Frozen:           wire.Frozen,
		ClosureUncertain: wire.ClosureUncertain,
	}
	var err error
	result.CheckedAt, err = time.Parse(time.RFC3339Nano, wire.CheckedAt)
	if err != nil {
		return result, err
	}
	if wire.SampledAt != nil {
		value, e := time.Parse(time.RFC3339Nano, *wire.SampledAt)
		if e != nil {
			return result, e
		}
		result.SampledAt = &value
	}
	return result, nil
}

func validateMemberRuntime(result MemberStorageRuntime, expected MemberIdentity) (int64, error) {
	if result.Member != expected.Member.String() || result.FailureDomain != expected.FailureDomain ||
		result.OwnerEpoch == nil ||
		result.BoundOwnerEpoch != nil {
		return 0, ErrConflict
	}
	epoch, err := strconv.ParseInt(*result.OwnerEpoch, 10, 64)
	if err != nil || epoch < 1 || storageDecimal(epoch) != *result.OwnerEpoch {
		return 0, ErrConflict
	}
	if err = validateRuntimeSample(result); err != nil {
		return 0, err
	}
	return epoch, nil
}

func validateRuntimeSample(result MemberStorageRuntime) error {
	switch result.Status {
	case "gate_busy":
		if runtimeHasGuardedFields(result) {
			return ErrConflict
		}
		return nil
	case "sampled":
		return validateRuntimeFields(result)
	default:
		return ErrConflict
	}
}

func runtimeHasGuardedFields(result MemberStorageRuntime) bool {
	return result.SampledAt != nil || result.CurrentFile != nil || result.Closed != nil || result.Frozen != nil ||
		result.ClosureUncertain != nil
}

func validateRuntimeFields(result MemberStorageRuntime) error {
	if result.SampledAt == nil || result.SampledAt.IsZero() || result.CurrentFile == nil || result.Closed == nil ||
		result.Frozen == nil ||
		result.ClosureUncertain == nil {
		return ErrConflict
	}
	file, err := idFrom(*result.CurrentFile)
	if err != nil || file == (ID{}) {
		return ErrConflict
	}
	return nil
}
