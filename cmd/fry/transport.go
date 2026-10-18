//go:build linux

package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Listener transport is explicit: port numbers do not select encryption.
type transportConfig struct {
	Transport               string `json:"transport"`
	CertificateFile         string `json:"tls_cert_file"`
	KeyFile                 string `json:"tls_key_file"`
	HandshakeTimeoutSeconds int    `json:"handshake_timeout_seconds"`
	tls                     *tls.Config
}

func (c *transportConfig) prepare(listen, role string) error {
	if c.Transport == "" {
		c.Transport = "loopback"
	}
	if !c.validRole(role) {
		return fmt.Errorf("%s: unsupported listener transport", role)
	}
	ip, err := listenerIP(listen, role)
	if err != nil {
		return err
	}
	if c.Transport == "loopback" {
		return c.prepareLoopback(ip, role)
	}
	return c.loadCertificate(role)
}

func (c transportConfig) handshakeTimeout() time.Duration {
	return time.Duration(c.HandshakeTimeoutSeconds) * time.Second
}

// Validate every edge before database initialization or starting workers. Keys
// are loaded once, so startup cannot race later per-listener file changes.
func (c *config) prepareListeners() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:2525"
	}
	if err := c.prepare(c.Listen, "incoming"); err != nil {
		return err
	}
	if c.IMAP != nil {
		if err := c.IMAP.prepare(c.IMAP.Listen, "imap"); err != nil {
			return err
		}
	}
	if c.Outbound != nil && c.Outbound.Submission != nil {
		s := c.Outbound.Submission
		if err := s.prepare(s.Listen, "submission"); err != nil {
			return err
		}
	}
	if c.Admin != nil {
		return c.Admin.prepareListener()
	}
	return nil
}

// Bound raw HTTP sockets, including clients stalled before a TLS handshake or
// request header. This wrapper sits below TLS so net/http sees *tls.Conn.
type admissionListener struct {
	net.Listener
	slots chan struct{}
}

func (l *admissionListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &admissionConn{Conn: conn, release: func() { <-l.slots }}, nil
		default:
			if err := conn.Close(); err != nil {
				log.Print("rejected administration socket close incomplete")
			}
		}
	}
}

type admissionConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *admissionConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

func (c transportConfig) validRole(role string) bool {
	valid := c.Transport == "loopback" || c.Transport == "tls"
	if role == "incoming" {
		valid = valid || c.Transport == "starttls"
	}
	if role == "submission" {
		valid = valid || c.Transport == "starttls-required"
	}
	return valid
}

func listenerIP(listen, role string) (net.IP, error) {
	host, port, err := net.SplitHostPort(listen)
	ip := net.ParseIP(host)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || ip == nil || portErr != nil || portNumber < 0 || portNumber > 65535 {
		return nil, fmt.Errorf("%s: listener requires numeric IP and port", role)
	}
	return ip, nil
}

func (c *transportConfig) prepareLoopback(ip net.IP, role string) error {
	if !ip.IsLoopback() || c.CertificateFile != "" || c.KeyFile != "" || c.HandshakeTimeoutSeconds != 0 {
		return fmt.Errorf("%s: loopback transport requires loopback IP and no TLS settings", role)
	}
	c.tls = nil
	return nil
}

func (c *adminConfig) prepareListener() error {
	a := c
	if err := a.prepare(a.Listen, "administration"); err != nil {
		return err
	}
	if err := a.validateOrigin(); err != nil {
		return err
	}
	if a.MaxConnections == 0 {
		a.MaxConnections = 64
	}
	if a.MaxConnections < 1 || a.MaxConnections > 4096 {
		return fmt.Errorf("administration: max_connections must be 1..4096")
	}
	return nil
}

func (c *adminConfig) validateOrigin() error {
	origin, err := parseAdminOrigin(c.Origin)
	if err != nil {
		return err
	}
	if c.Transport == "tls" && origin.Scheme != "https" {
		return fmt.Errorf("administration: TLS listener requires HTTPS origin")
	}
	if c.Transport == "loopback" && (origin.Scheme != "http" || !net.ParseIP(origin.Hostname()).IsLoopback()) {
		return fmt.Errorf("administration: loopback listener requires HTTP loopback origin")
	}
	return nil
}

func (c *transportConfig) loadCertificate(role string) error {
	if c.HandshakeTimeoutSeconds == 0 {
		c.HandshakeTimeoutSeconds = 10
	}
	if c.HandshakeTimeoutSeconds < 1 || c.HandshakeTimeoutSeconds > 60 {
		return fmt.Errorf("%s: TLS handshake timeout must be 1..60 seconds", role)
	}
	if c.CertificateFile == "" || c.KeyFile == "" {
		return fmt.Errorf("%s: TLS certificate and key files required", role)
	}
	pair, err := tls.LoadX509KeyPair(c.CertificateFile, c.KeyFile)
	if err != nil {
		return fmt.Errorf("%s: cannot load TLS certificate/key pair: %w", role, err)
	}
	c.tls = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{pair},
	}
	return nil
}

func parseAdminOrigin(raw string) (*url.URL, error) {
	origin, err := url.Parse(raw)
	if err != nil || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery ||
		origin.Fragment != "" ||
		origin.Path != "" {
		return nil, fmt.Errorf("administration: invalid origin")
	}
	return origin, nil
}
