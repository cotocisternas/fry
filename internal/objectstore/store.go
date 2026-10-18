// Package objectstore implements the immutable-pack object contract. Object keys
// are never reused. Retirement leaves permanent empty objects, never delete markers.
package objectstore

import (
	"context"
	"errors"
	"io"
)

var (
	ErrMissing   = errors.New("objectstore: object missing")
	ErrConflict  = errors.New("objectstore: precondition conflict")
	ErrIntegrity = errors.New("objectstore: size, checksum, or generation mismatch")
)

type Info struct {
	Size    int64
	SHA256  [32]byte
	ETag    string
	Retired bool
}
type Store interface {
	// Namespace identifies the physical object address and must remain stable.
	Namespace() string
	Create(context.Context, string, io.ReadSeeker, int64, [32]byte) error
	Stat(context.Context, string) (Info, error)
	Range(context.Context, string, string, int64, int64) (io.ReadCloser, error)
	Retire(context.Context, string) error
}
