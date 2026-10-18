//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"io"

	"github.com/cotocisternas/fry/internal/hotstore"
)

// copyHotRecord streams one complete record from a current store or sealed image
// into a tracked successor. Closing the reader on append failure releases a
// producer blocked in Write; joining it before returning keeps source I/O inside
// the caller's member gate and image lifetime. The caller retains ownership of
// both files and must publish nothing unless the copied size and digest match.
func copyHotRecord(
	ctx context.Context,
	next *hotstore.Store,
	original hotstore.Locator,
	read func(context.Context, hotstore.Locator, int64, int64, io.Writer) error,
) (hotstore.Locator, error) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := read(ctx, original, 0, original.Size, writer)
		finishPipe(writer, err)
		done <- err
	}()
	loc, err := next.Append(ctx, original.Key, reader)
	finishPipe(reader, err)
	readErr := <-done
	if err != nil || readErr != nil {
		return loc, errors.Join(err, readErr)
	}
	if loc.Size != original.Size || loc.SHA256 != original.SHA256 {
		return loc, ErrConflict
	}
	return loc, nil
}
