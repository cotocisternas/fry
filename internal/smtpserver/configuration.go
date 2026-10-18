package smtpserver

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"slices"
	"time"

	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/emersion/go-smtp"
)

// Transport modes govern listener encryption and when SMTP authentication is allowed.
const (
	transportLoopback         = "loopback"
	transportSTARTTLS         = "starttls"
	transportSTARTTLSRequired = "starttls-required"
	transportImplicitTLS      = "tls"
)

func validServiceRole(c Config) bool {
	incoming := len(c.Routes) > 0 || c.Directory != nil
	return domainValid(c.Domain) && (incoming || c.Submission != nil) && (!incoming || c.Submission == nil) &&
		(len(c.Routes) == 0 || c.Directory == nil)
}

func validServiceBounds(c Config) bool {
	return c.MaxConnections >= 1 && c.MaxInFlight >= 1 && c.MaxRecipients >= 1 && c.MaxRecipients <= 1024 &&
		c.MaxMessageBytes >= 1 &&
		c.MaxMessageBytes <= 256<<20 &&
		c.IdleTimeout >= time.Second &&
		c.IngestTimeout >= time.Second
}

func normalizeTransport(config Config) (Config, error) {
	if config.Transport == "" {
		config.Transport = transportLoopback
	}
	switch config.Transport {
	case transportLoopback:
		if config.TLSConfig != nil {
			return config, fmt.Errorf("smtpserver: loopback transport must not configure TLS")
		}
	case transportSTARTTLS, transportSTARTTLSRequired, transportImplicitTLS:
		return normalizeTLS(config)

	default:
		return config, fmt.Errorf("smtpserver: invalid transport")
	}
	return config, nil
}

func normalizeTLS(config Config) (Config, error) {
	if config.Transport == transportSTARTTLS && config.Submission != nil ||
		config.Transport == transportSTARTTLSRequired && config.Submission == nil {
		return config, fmt.Errorf("smtpserver: TLS mode does not match service role")
	}
	if !validTLSCertificate(config.TLSConfig) {
		return config, fmt.Errorf("smtpserver: TLS certificate required")
	}

	config.TLSConfig = config.TLSConfig.Clone()
	if config.TLSConfig.MinVersion < tls.VersionTLS12 {
		config.TLSConfig.MinVersion = tls.VersionTLS12
	}
	if config.TLSConfig.MaxVersion != 0 && config.TLSConfig.MaxVersion < config.TLSConfig.MinVersion {
		return config, fmt.Errorf("smtpserver: invalid TLS version bounds")
	}
	return normalizeHandshake(config)
}

func normalizeHandshake(config Config) (Config, error) {
	if config.HandshakeTimeout == 0 {
		config.HandshakeTimeout = 10 * time.Second
	}
	if config.HandshakeTimeout < time.Second || config.HandshakeTimeout > time.Minute {
		return config, fmt.Errorf("smtpserver: invalid TLS handshake timeout")
	}
	return config, nil
}

func (s *Server) configureRoutes(routes []Route) error {
	domains := map[string]receiptstore.ID{}
	for _, route := range routes {
		key, domain, err := validateRoute(route)
		if err != nil {
			return err
		}
		if _, ok := s.routes[key]; ok {
			return fmt.Errorf("smtpserver: duplicate address")
		}
		if old, ok := domains[domain]; ok && old != route.Organization {
			return fmt.Errorf("smtpserver: domain assigned to multiple organizations")
		}
		domains[domain] = route.Organization
		s.routes[key] = resolved{route.Organization, append([]receiptstore.ID(nil), route.Inboxes...)}
	}
	return nil
}

func validateRoute(route Route) (string, string, error) {
	key, domain, err := addressKey(route.Address)
	if err != nil || route.Organization == (receiptstore.ID{}) || len(route.Inboxes) == 0 || len(route.Inboxes) > 1024 {
		return "", "", fmt.Errorf("smtpserver: invalid route")
	}
	if slices.Contains(route.Inboxes, receiptstore.ID{}) {
		return "", "", fmt.Errorf("smtpserver: invalid inbox")
	}
	return key, domain, nil
}

func (s *Server) configureWireServer() {
	config := s.config
	s.smtp = smtp.NewServer(
		smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) { return &session{server: s, conn: c.Conn()}, nil }),
	)
	s.smtp.Domain = config.Domain
	s.smtp.MaxRecipients = config.MaxRecipients
	s.smtp.MaxMessageBytes = config.MaxMessageBytes
	s.smtp.ReadTimeout = config.IdleTimeout
	s.smtp.WriteTimeout = config.IdleTimeout
	// Plaintext authentication is confined to explicit local fixture mode.
	s.smtp.AllowInsecureAuth = config.Submission != nil && config.Transport == transportLoopback
	s.smtp.TLSConfig = config.TLSConfig
	s.smtp.TLSHandshakeTimeout = config.HandshakeTimeout
	s.smtp.RequireTLS = config.Transport == transportSTARTTLSRequired
}

func (s *Server) rejectConnection(raw net.Conn) {
	defer closeAfterFailure(raw)
	if s.config.Transport == transportImplicitTLS {
		return
	}
	if err := raw.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return
	}
	if _, err := io.WriteString(raw, "421 4.3.2 FRY connection capacity reached\r\n"); err != nil {
		return
	}
}

func validTLSCertificate(tc *tls.Config) bool {
	return tc != nil && tc.GetConfigForClient == nil && (len(tc.Certificates) > 0 || tc.GetCertificate != nil)
}
