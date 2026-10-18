package smtpserver

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/emersion/go-smtp"
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
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, pgx.ErrTxClosed) &&
			!errors.Is(err, smtp.ErrServerClosed) {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				t.Errorf("fixture cleanup failed (%T): context deadline exceeded", err)
			case errors.Is(err, context.Canceled):
				t.Errorf("fixture cleanup failed (%T): context canceled", err)
			default:
				var transportErr *net.OpError
				if errors.As(err, &transportErr) {
					t.Errorf("fixture transport cleanup failed (%T): %v", err, transportErr)
				} else {
					t.Errorf("fixture cleanup failed (%T)", err)
				}
			}
		}
	}
}
