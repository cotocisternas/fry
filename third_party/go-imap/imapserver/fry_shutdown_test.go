package imapserver

import (
	"net"
	"sync"
	"testing"
	"time"
)

type closeRaceListener struct {
	done chan struct{}
	once sync.Once
}

func (l *closeRaceListener) Accept() (net.Conn, error) { <-l.done; return nil, net.ErrClosed }
func (l *closeRaceListener) Close() error              { l.once.Do(func() { close(l.done) }); return nil }
func (l *closeRaceListener) Addr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func TestServeCloseRegistrationRace(t *testing.T) {
	for i := 0; i < 10000; i++ {
		s := New(&Options{})
		l := &closeRaceListener{done: make(chan struct{})}
		done := make(chan struct{})
		go func() { _ = s.Serve(l); close(done) }()
		_ = s.Close()
		_ = l.Close() // Serve may have rejected this caller-owned listener.
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Serve survived Close")
		}
	}
}

func TestClosedServerRejectsLateConnection(t *testing.T) {
	s := New(&Options{NewSession: func(*Conn) (Session, *GreetingData, error) {
		t.Error("session created after shutdown")
		return nil, nil, nil
	}})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() { newConn(server, s).serve(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("late connection survived shutdown")
	}
}
