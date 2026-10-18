//go:build linux

package receiptstore

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

type memberTLSValidity struct {
	notBefore, notAfter time.Time
}

// Guard the socket below TLS so neither pooled HTTP/2 headers nor original bytes
// can reach the network after peer expiry. The interval is immutable once the
// handshake has verified the chain; the unarmed phase permits only handshake I/O.
// Returning the concrete *tls.Conn preserves net/http's HTTP/2 ALPN handling.
type memberTLSWriteGuard struct {
	net.Conn
	validity atomic.Pointer[memberTLSValidity]
}

func (c *memberTLSWriteGuard) Write(p []byte) (int, error) {
	if validity := c.validity.Load(); validity != nil {
		now := time.Now()
		if now.Before(validity.notBefore) || !now.Before(validity.notAfter) {
			return 0, ErrStale
		}
	}
	return c.Conn.Write(p)
}

func memberVerifiedValidity(state tls.ConnectionState) (*memberTLSValidity, error) {
	if state.NegotiatedProtocol != "h2" || !validMemberCertificates(&state) {
		return nil, ErrStale
	}
	chain := state.VerifiedChains[0]
	validity := &memberTLSValidity{notBefore: chain[0].NotBefore, notAfter: chain[0].NotAfter}
	for _, certificate := range chain[1:] {
		if certificate.NotBefore.After(validity.notBefore) {
			validity.notBefore = certificate.NotBefore
		}
		if certificate.NotAfter.Before(validity.notAfter) {
			validity.notAfter = certificate.NotAfter
		}
	}
	return validity, nil
}

type memberTLSDialer struct {
	config *tls.Config
	dialer net.Dialer
}

func (d *memberTLSDialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	hostname, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := d.dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	guard := &memberTLSWriteGuard{Conn: raw}
	config := d.config.Clone()
	if config.ServerName == "" {
		config.ServerName = hostname
	}
	conn := tls.Client(guard, config)
	if err = conn.HandshakeContext(ctx); err != nil {
		return nil, errors.Join(err, raw.Close())
	}
	validity, err := memberVerifiedValidity(conn.ConnectionState())
	if err != nil {
		return nil, errors.Join(err, raw.Close())
	}
	guard.validity.Store(validity)
	return conn, nil
}

func newMemberTransport(opts RemoteMemberOptions) *http.Transport {
	config := memberClientTLS(opts)
	dialer := &memberTLSDialer{
		config: config,
		dialer: net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
	}
	return &http.Transport{
		TLSClientConfig:        config,
		HTTP2:                  memberHTTP2(0),
		ForceAttemptHTTP2:      true,
		Proxy:                  nil,
		DisableCompression:     true,
		MaxConnsPerHost:        opts.MaxConnections,
		MaxIdleConns:           opts.MaxConnections,
		MaxIdleConnsPerHost:    opts.MaxConnections,
		IdleConnTimeout:        time.Minute,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  opts.Timeout,
		MaxResponseHeaderBytes: 16 << 10,
		DialTLSContext:         dialer.dial,
	}
}
