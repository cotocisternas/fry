package receiptstore

import "fmt"

// Static statements are defined in db/queries/receiptstore/ and checked by sqlc.
// Call sites use typed methods for writes, single rows, and bounded batches.
// Existing streaming scans use sqlc's exported statement constants to keep their incremental
// decoding, early-close behavior, and transaction/lock lifetime. MemberStorage's
// lateral owner snapshot also scans the exported statement: sqlc v1.31.1 does
// not infer the outer join's nullable owner epoch/closed values correctly.
// Schema installation and transaction settings/table locks remain explicit.

// sqlcPointer adapts a required application value to an optional query parameter.
func sqlcPointer[T any](value T) *T { return &value }

// sqlcUint32 preserves pgx's checked narrowing when the public API uses UIDs.
func sqlcUint32[T ~int16 | ~int32 | ~int64](value T) (uint32, error) {
	if value < 0 || uint64(value) > 1<<32-1 {
		return 0, fmt.Errorf("receiptstore: stored value outside uint32 range: %d", value)
	}
	return uint32(value), nil
}

func sqlcUint64[T ~int16 | ~int32 | ~int64](value T) (uint64, error) {
	if value < 0 {
		return 0, fmt.Errorf("receiptstore: stored negative unsigned value: %d", value)
	}
	return uint64(value), nil
}
