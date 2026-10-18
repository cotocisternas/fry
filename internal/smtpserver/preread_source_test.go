package smtpserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
)

type prereadConn struct {
	net.Conn
	closes atomic.Int32
}

func (c *prereadConn) Close() error { c.closes.Add(1); return c.Conn.Close() }

type prereadReader struct{ read func([]byte) (int, error) }

func (r prereadReader) Read(p []byte) (int, error) { return r.read(p) }
func prereadSocket(t *testing.T) *prereadConn {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { testCleanup(t)(a.Close()); testCleanup(t)(b.Close()) })
	return &prereadConn{Conn: a}
}

func prereadWait(t *testing.T, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(time.Second):
		t.Fatal("blocked source operation")
	}
}

func TestPrereadSourceCloseBeforeReadPreservesOnlyBoundedDrain(t *testing.T) {
	conn := prereadSocket(t)
	var reads atomic.Int32
	body := bytes.NewBufferString("exact body")
	source := &messageSource{
		conn:   conn,
		Reader: prereadReader{read: func(p []byte) (int, error) { reads.Add(1); return body.Read(p) }},
	}
	testCleanup(t)(source.Close())
	testCleanup(t)(source.Close())
	requireTestCondition(t, conn.closes.Load() == 0, "untouched DATA socket closed")
	{
		_, err := source.Read(make([]byte, 1))
		requireTestCondition(
			t,
			(errors.Is(err, io.ErrClosedPipe) && reads.Load() == 0),
			"Read started after Close",
			err,
			reads.Load(),
		)
	}
	{
		err := source.drainUntouched(context.Background(), 10)
		requireTestCondition(t, err == nil, err)
	}
	source.finishDeferred()
	source.finishDeferred()
	requireTestCondition(t, conn.closes.Load() == 0, "proven complete framing closed socket")
}

func TestPrereadSourceReadStartedCloseStillUnblocks(t *testing.T) {
	conn := prereadSocket(t)
	started, done := make(chan struct{}), make(chan struct{})
	source := &messageSource{
		conn:   conn,
		Reader: prereadReader{read: func(p []byte) (int, error) { close(started); return conn.Read(p) }},
	}
	go func() {
		defer close(done)
		if _, err := source.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("blocked read: %v", err)
		}
	}()
	prereadWait(t, started)
	testCleanup(t)(source.Close())
	testCleanup(t)(source.Close())
	prereadWait(t, done)
	source.finishDeferred()
	requireTestCondition(
		t,
		conn.closes.Load() == 1,
		"blocked DATA reader not interrupted exactly once",
		conn.closes.Load(),
	)
}

func TestPrereadSourceBDATCloseUnchanged(t *testing.T) {
	conn := prereadSocket(t)
	reader, writer := io.Pipe()
	defer func() {
		testCleanup(t)(writer.Close())
	}()
	source := &messageSource{conn: conn, Reader: reader}
	testCleanup(t)(source.Close())
	{
		_, err := writer.Write([]byte("chunk"))
		requireTestCondition(t, errors.Is(err, io.ErrClosedPipe), "BDAT pipe not closed", err)
	}
	{
		err := source.drainUntouched(context.Background(), 1024)
		requireTestCondition(t, err == nil, "BDAT behavior changed", err)
	}
	source.finishDeferred()
	requireTestCondition(t, conn.closes.Load() == 0, "BDAT close unexpectedly closed raw socket")
}

func TestPrereadSourceRejectsMissingFramingProof(t *testing.T) {
	for _, mode := range []string{"oversize", "truncated", "zero-progress", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			conn := prereadSocket(t)
			var reader io.Reader = bytes.NewBufferString("one byte too many")
			switch mode {
			case "truncated":
				reader = prereadReader{read: func([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }}
			case "zero-progress":
				reader = prereadReader{read: func([]byte) (int, error) { return 0, nil }}
			}
			source := &messageSource{conn: conn, Reader: reader}
			testCleanup(t)(source.Close())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			err := source.drainUntouched(ctx, 4)
			requireTestCondition(t, err != nil, "missing EOF proof accepted")
			requireTestCondition(t, (mode != "oversize" || errors.Is(err, hotstore.ErrTooLarge)), err)
			source.finishDeferred()
			source.finishDeferred()
			requireTestCondition(t, conn.closes.Load() == 1, "unsafe framing left socket reusable", conn.closes.Load())
		})
	}
}

func TestPrereadSourceStalledDrainUsesExistingContextSocketCancellation(t *testing.T) {
	conn := prereadSocket(t)
	started, done := make(chan struct{}), make(chan struct{})
	source := &messageSource{
		conn:   conn,
		Reader: prereadReader{read: func(p []byte) (int, error) { close(started); return conn.Read(p) }},
	}
	testCleanup(t)(source.Close())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := context.AfterFunc(ctx, func() { testCleanup(t)(conn.Close()) })
	defer stop()
	var drainErr error
	go func() { defer close(done); drainErr = source.drainUntouched(ctx, 1024) }()
	prereadWait(t, started)
	cancel()
	prereadWait(t, done)
	requireTestCondition(t, drainErr != nil, "stalled DATA became a framed rejection")
	source.finishDeferred()
}

func TestPrereadSourceUnresolvedOrAcceptedUntouchedDisconnects(t *testing.T) {
	conn := prereadSocket(t)
	source := &messageSource{conn: conn, Reader: bytes.NewBufferString("not consumed")}
	testCleanup(t)(source.Close())
	// All exits lacking proven EOF use the same finalizer, without inventing a
	// negative reply for accepted replay or an unresolved receipt outcome.
	source.finishDeferred()
	source.finishDeferred()
	requireTestCondition(t, conn.closes.Load() == 1, "untouched exit left vendor free to drain after deadline release")
}
