//go:build linux

package imapserver

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"testing"

	wire "github.com/emersion/go-imap/v2/imapserver"
)

type closeRaceListener struct {
	done chan struct{}
	once sync.Once
}

func (l *closeRaceListener) Accept() (net.Conn, error) { <-l.done; return nil, net.ErrClosed }
func (l *closeRaceListener) Close() error              { l.once.Do(func() { close(l.done) }); return nil }

func (l *closeRaceListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
}

func TestConcurrentServeAndCloseJoinAllHandshakes(t *testing.T) {
	for range 30000 {
		ctx, cancel := context.WithCancel(context.Background())
		s := &Server{
			ctx:         ctx,
			cancel:      cancel,
			connections: map[*transportConn]struct{}{},
			transport:   Options{Transport: "tls", TLS: &tls.Config{}, MaxPendingHandshakes: 1},
			wire:        wire.New(&wire.Options{}),
		}
		l := &closeRaceListener{done: make(chan struct{})}
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			testCleanup(t)(s.Serve(l))
		}()
		go func() {
			defer wg.Done()
			testCleanup(t)(s.Close())
		}()
		go func() {
			defer wg.Done()
			testCleanup(t)(s.Close())
		}()
		wg.Wait()
		testCleanup(t)(l.Close())
	}
}
