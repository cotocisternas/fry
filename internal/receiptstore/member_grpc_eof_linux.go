//go:build linux

package receiptstore

import (
	"io"

	"connectrpc.com/connect/v2"
)

// Connect v2.0.1's HTTP/gRPC envelope reader wraps clean EOF in CodeUnknown
// with a direct io.EOF cause. Normalize only that transport representation.
// Searching an error tree could discard an accompanying failure or status.
func memberReceiveError(err error) error {
	//nolint:errorlint // Only the outer SDK wrapper is eligible for normalization.
	coded, ok := err.(*connect.Error)
	if !ok || coded.Code() != connect.CodeUnknown || coded.Message() != "EOF" || coded.IsRemote() ||
		len(coded.Details()) != 0 {
		return err
	}
	if coded.Unwrap() == io.EOF { //nolint:errorlint // The direct cause must be the exact EOF sentinel.
		return io.EOF
	}
	return err
}
