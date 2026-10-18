package smtp

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type lateListener struct {
	conn             net.Conn
	entered, release chan struct{}
	once             sync.Once
}

func (l *lateListener) Accept() (net.Conn, error) {
	l.once.Do(func() { close(l.entered) })
	<-l.release
	return l.conn, nil
}
func (l *lateListener) Close() error   { return nil }
func (l *lateListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func TestFRYShutdownRejectsLateAccept(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	l := &lateListener{conn: a, entered: make(chan struct{}), release: make(chan struct{})}
	s := NewServer(nil)
	served := make(chan error, 1)
	go func() { served <- s.Serve(l) }()
	<-l.entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	close(l.release)
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("late Accept kept Serve alive")
	}
	b.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	if n, err := b.Read(buf); n != 0 || err == nil {
		t.Fatalf("late connection admitted: %d %v", n, err)
	}
}
func TestFRYServeAfterShutdownClosesListener(t *testing.T) {
	s := NewServer(nil)
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Serve(l); !errors.Is(err, ErrServerClosed) {
		t.Fatal(err)
	}
	if _, err = l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatal("listener leaked", err)
	}
}

type delayedDataSession struct {
	entered, release, finished, loggedOut chan struct{}
	logoutOnce                            sync.Once
}

func (s *delayedDataSession) Reset() {}
func (s *delayedDataSession) Logout() error {
	s.logoutOnce.Do(func() { close(s.loggedOut) })
	return nil
}
func (s *delayedDataSession) Mail(string, *MailOptions) error { return nil }
func (s *delayedDataSession) Rcpt(string, *RcptOptions) error { return nil }
func (s *delayedDataSession) Data(r io.Reader) error {
	close(s.entered)
	<-s.release
	_, err := io.Copy(io.Discard, r)
	close(s.finished)
	return err
}
func TestFRYBDATResetJoinsBackendBeforeReuse(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	go io.Copy(io.Discard, b)
	session := &delayedDataSession{entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{}), loggedOut: make(chan struct{})}
	c := newConn(a, NewServer(nil))
	c.session = session
	c.fromReceived = true
	c.recipients = []string{"recipient@example.test"}
	c.handleBdat("0")
	<-session.entered
	reset := make(chan struct{})
	go func() { c.reset(); close(reset) }()
	select {
	case <-reset:
		t.Fatal("Reset returned with Data still active")
	case <-time.After(20 * time.Millisecond):
	}
	close(session.release)
	select {
	case <-reset:
	case <-time.After(time.Second):
		t.Fatal("Reset did not join Data")
	}
	select {
	case <-session.finished:
	default:
		t.Fatal("Data did not finish")
	}
	c.Close()
}
func TestFRYBDATClosePreservesRunningSession(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	go io.Copy(io.Discard, b)
	session := &delayedDataSession{entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{}), loggedOut: make(chan struct{})}
	c := newConn(a, NewServer(nil))
	c.session = session
	c.fromReceived = true
	c.recipients = []string{"recipient@example.test"}
	c.handleBdat("0")
	<-session.entered
	c.Close()
	<-session.loggedOut
	joined := make(chan struct{})
	go func() { c.waitData(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("teardown lost active backend")
	case <-time.After(20 * time.Millisecond):
	}
	close(session.release)
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("backend did not complete")
	}
}
