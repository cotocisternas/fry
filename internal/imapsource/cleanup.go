package imapsource

import (
	"errors"
	"io"
	"log/slog"
	"net"
)

// closeAfterFailure releases an already rejected or interrupted operation.
// Cleanup errors are diagnostic only: they cannot change a committed result.
func closeAfterFailure(c io.Closer) {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Debug("cleanup failed", "operation", "close")
	}
}
