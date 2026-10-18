//go:build linux

package imapserver

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cotocisternas/fry/internal/telemetry"
)

const (
	transportLoopback    = "loopback"
	transportImplicitTLS = "tls"
)

// Options selects a plaintext loopback development listener or implicit TLS.
// Serve always receives a raw TCP listener; it owns admission and TLS wrapping.
type Options struct {
	Telemetry            *telemetry.Runtime
	Transport            string
	TLS                  *tls.Config
	MaxConnections       int
	MaxPendingHandshakes int
	HandshakeTimeout     time.Duration
}

func normalizeTransport(o Options) (Options, error) {
	o = defaultTransport(o)
	if !validTransportBounds(o) {
		return o, fmt.Errorf("imap: invalid bounded transport options")
	}

	switch o.Transport {
	case transportLoopback:
		if o.TLS != nil {
			return o, fmt.Errorf("imap: TLS requires explicit tls transport")
		}
	case transportImplicitTLS:
		if o.TLS == nil || len(o.TLS.Certificates) == 0 || o.TLS.GetConfigForClient != nil {
			return o, fmt.Errorf("imap: explicit TLS certificate required")
		}
		o.TLS = o.TLS.Clone()
		if o.TLS.MinVersion < tls.VersionTLS12 {
			o.TLS.MinVersion = tls.VersionTLS12
		}
	default:
		return o, fmt.Errorf("imap: unsupported transport")
	}
	return o, nil
}

type admissionListener struct {
	net.Listener
	server *Server
}

func (l *admissionListener) Accept() (net.Conn, error) {
	for {
		raw, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		l.server.mu.Lock()
		if l.server.closing || len(l.server.connections) >= l.server.transport.MaxConnections {
			l.server.mu.Unlock()
			closeAfterFailure(raw)
			continue
		}
		c := &transportConn{Conn: raw, server: l.server}
		l.server.connections[c] = struct{}{}
		l.server.mu.Unlock()
		return c, nil
	}
}

type transportConn struct {
	net.Conn
	server *Server
	once   sync.Once
	err    error
}

func (c *transportConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.server.mu.Lock()
		delete(c.server.connections, c)
		c.server.mu.Unlock()
	})
	return c.err
}

type handshakeResult struct {
	connection net.Conn
	err        error
}
type handshakeListener struct {
	net.Listener
	server  *Server
	ctx     context.Context
	cancel  context.CancelFunc
	once    sync.Once
	results chan handshakeResult
	pending chan struct{}
}

func newHandshakeListener(s *Server, l net.Listener) *handshakeListener {
	ctx, cancel := context.WithCancel(s.ctx)
	return &handshakeListener{
		Listener: l,
		server:   s,
		ctx:      ctx,
		cancel:   cancel,
		results:  make(chan handshakeResult),
		pending:  make(chan struct{}, s.transport.MaxPendingHandshakes),
	}
}

func (l *handshakeListener) Accept() (net.Conn, error) {
	l.once.Do(func() { l.server.handshakes.Add(1); go l.acceptLoop() })
	select {
	case result := <-l.results:
		return result.connection, result.err
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
}

func (l *handshakeListener) acceptLoop() {
	defer l.server.handshakes.Done()
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			select {
			case l.results <- handshakeResult{err: err}:
			case <-l.ctx.Done():
			}
			return
		}
		select {
		case l.pending <- struct{}{}:
		default:
			closeAfterFailure(connection)
			continue
		}
		l.server.handshakes.Add(1)
		go l.handshake(connection)
	}
}
func (l *handshakeListener) Close() error { l.cancel(); return l.Listener.Close() }

func defaultTransport(o Options) Options {
	if o.Transport == "" {
		o.Transport = transportLoopback
	}
	if o.MaxConnections == 0 {
		o.MaxConnections = 64
	}
	if o.MaxPendingHandshakes == 0 {
		o.MaxPendingHandshakes = 16
	}
	if o.HandshakeTimeout == 0 {
		o.HandshakeTimeout = 10 * time.Second
	}
	return o
}

func validTransportBounds(o Options) bool {
	return o.MaxConnections >= 1 && o.MaxConnections <= 4096 && o.MaxPendingHandshakes >= 1 &&
		o.MaxPendingHandshakes <= 4096 &&
		o.HandshakeTimeout >= time.Millisecond &&
		o.HandshakeTimeout <= time.Minute
}

func (l *handshakeListener) handshake(connection net.Conn) {
	defer l.server.handshakes.Done()
	defer func() { <-l.pending }()
	ctx, cancel := context.WithTimeout(l.ctx, l.server.transport.HandshakeTimeout)
	defer cancel()
	encrypted, ok := connection.(*tls.Conn)
	if !ok || encrypted.HandshakeContext(ctx) != nil {
		closeAfterFailure(connection)
		return
	}
	// A concrete *tls.Conn reaches go-imap, which gates LOGIN on TLS state.
	select {
	case l.results <- handshakeResult{connection: encrypted}:
	case <-l.ctx.Done():
		closeAfterFailure(connection)
	}
}
