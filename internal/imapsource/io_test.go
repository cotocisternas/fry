package imapsource

import (
	"errors"
	"net"
	"testing"

	"github.com/jackc/pgx/v5"
)

func testCheck(t *testing.T) func(error) {
	t.Helper()
	return func(err error) {
		t.Helper()
		if err != nil {
			t.Errorf("fixture I/O: %v", err)
		}
	}
}

func testResult(t *testing.T) func(any, error) {
	t.Helper()
	return func(_ any, err error) { t.Helper(); testCheck(t)(err) }
}

// Shutdown and repeated cleanup may observe a transport already closed by the
// server, or a transaction already committed. Other cleanup failures fail tests.
func testCleanup(t *testing.T) func(error) {
	t.Helper()
	return func(err error) {
		t.Helper()
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error("fixture cleanup failed")
		}
	}
}
