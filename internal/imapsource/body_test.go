package imapsource

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

type blockedReader struct{ started, closed chan struct{} }

func (r blockedReader) Read([]byte) (int, error) {
	close(r.started)
	<-r.closed
	return 0, io.ErrClosedPipe
}

func TestBodyCloseUnblocksRead(t *testing.T) {
	started, closed := make(chan struct{}), make(chan struct{})
	body := &verifiedLiteral{
		reader:    blockedReader{started, closed},
		remaining: 3,
		conn:      closeFunc(func() error { close(closed); return nil }),
	}
	done := make(chan error)
	go func() { _, err := body.Read(make([]byte, 3)); done <- err }()
	<-started
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBodyCloseDuringCompletionCannotVerify(t *testing.T) {
	started, resume := make(chan struct{}), make(chan struct{})
	body := &verifiedLiteral{
		reader:    strings.NewReader(""),
		remaining: 0,
		conn:      closeFunc(func() error { return nil }),
		finalize:  func() error { close(started); <-resume; return nil },
	}
	done := make(chan error)
	go func() { _, err := body.Read(make([]byte, 1)); done <- err }()
	<-started
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	close(resume)
	if err := <-done; !errors.Is(err, ErrSessionClosed) {
		t.Fatal(err)
	}
	if body.state.Load() == bodyVerified {
		t.Fatal("closed stream became verified")
	}
}
