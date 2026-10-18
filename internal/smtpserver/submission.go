package smtpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cotocisternas/fry/internal/credentials"
	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

type SubmissionAccount struct {
	Username, Password                   string
	Organization                         receiptstore.ID
	AuthorizedSenders, AuthorizedDomains []string
}

// SenderAuthority supplies current organization-scoped domain/address grants.
type SenderAuthority interface {
	AuthorizeSender(context.Context, receiptstore.ID, string) (*receiptstore.SenderAuthorization, error)
}

// SubmissionCredentials authenticates and revalidates a persisted generation.
type SubmissionCredentials interface {
	AuthenticateSubmission(context.Context, string, string) (maildirectory.SubmissionPrincipal, error)
	ValidateSubmission(context.Context, maildirectory.SubmissionPrincipal) error
}

// SubmissionConfig combines configured sender restrictions with live authority.
type SubmissionConfig struct {
	Credentials        SubmissionCredentials
	SenderAuthority    SenderAuthority
	Accounts           []SubmissionAccount
	DefaultRelay       receiptstore.ID
	OrganizationRelays map[receiptstore.ID]receiptstore.ID
	RetryWindow        time.Duration
}
type principal struct {
	org, relay       receiptstore.ID
	credential       *maildirectory.SubmissionPrincipal
	senders, domains map[string]bool
}

func (s *Server) configureSubmission(config SubmissionConfig) error {
	if !validSubmissionAuthority(s.config.Transport, config) {
		return fmt.Errorf("smtpserver: public submission requires live sender and credential authority")
	}

	if config.RetryWindow < time.Second || config.RetryWindow > 30*24*time.Hour || config.RetryWindow%time.Second != 0 {
		return fmt.Errorf("smtpserver: explicit bounded outbound retry window required")
	}
	if config.Credentials != nil {
		return s.configureLiveSubmission(config)
	}

	if len(config.Accounts) == 0 {
		return fmt.Errorf("smtpserver: submission accounts required")
	}
	s.retryWindow = config.RetryWindow
	s.accounts = map[string]principal{}
	passwords := map[string]string{}
	for _, account := range config.Accounts {
		if err := s.addSubmissionAccount(config, account, passwords); err != nil {
			return err
		}
	}

	var err error
	s.passwords, err = credentials.New(passwords)
	return err
}

func (s *session) AuthMechanisms() []string {
	if s.server.passwords == nil &&
		(s.server.config.Submission == nil || s.server.config.Submission.Credentials == nil) {
		return nil
	}
	return []string{sasl.Plain}
}

func (s *session) Auth(mechanism string) (sasl.Server, error) {
	if mechanism != sasl.Plain ||
		(s.server.passwords == nil &&
			(s.server.config.Submission == nil ||
				s.server.config.Submission.Credentials == nil)) {
		return nil, smtp.ErrAuthUnsupported
	}
	return sasl.NewPlainServer(s.authenticatePlain), nil
}

func (s *session) validateCredential(ctx context.Context, p *maildirectory.SubmissionPrincipal) error {
	provider := s.server.config.Submission.Credentials
	if provider == nil {
		return nil
	}
	if p == nil {
		return receiptstore.ErrSubmissionUnauthorized
	}
	err := provider.ValidateSubmission(ctx, *p)
	if errors.Is(err, maildirectory.ErrNotFound) {
		return receiptstore.ErrSubmissionUnauthorized
	}
	return err
}

func submissionUnauthorized() error {
	return &smtp.SMTPError{
		Code:         535,
		EnhancedCode: smtp.EnhancedCode{5, 7, 8},
		Message:      "Submission credential is no longer authorized; reconnect and authenticate again",
	}
}

func (s *Server) configureLiveSubmission(config SubmissionConfig) error {
	if config.DefaultRelay == (receiptstore.ID{}) {
		return fmt.Errorf("smtpserver: default relay required")
	}
	relays := make(map[receiptstore.ID]receiptstore.ID, len(config.OrganizationRelays))
	for org, relay := range config.OrganizationRelays {
		if org == (receiptstore.ID{}) || relay == (receiptstore.ID{}) {
			return fmt.Errorf("smtpserver: invalid organization relay")
		}
		relays[org] = relay
	}
	// Listener relay selection is immutable; authorization remains live.
	config.OrganizationRelays = relays
	s.config.Submission = &config
	s.retryWindow = config.RetryWindow
	return nil
}

func (s *Server) addSubmissionAccount(config SubmissionConfig, a SubmissionAccount, passwords map[string]string) error {
	if a.Username == "" || len(a.Username) > 128 || strings.TrimSpace(a.Username) != a.Username ||
		strings.ContainsAny(a.Username, "\r\n\x00") {
		return fmt.Errorf("smtpserver: invalid submission username")
	}
	if _, ok := s.accounts[a.Username]; ok {
		return fmt.Errorf("smtpserver: duplicate submission user")
	}
	relay := config.DefaultRelay
	if override, ok := config.OrganizationRelays[a.Organization]; ok {
		relay = override
	}
	if a.Organization == (receiptstore.ID{}) || relay == (receiptstore.ID{}) {
		return fmt.Errorf("smtpserver: organization and selected relay required")
	}
	p, err := configuredPrincipal(a, relay)
	if err != nil {
		return err
	}
	s.accounts[a.Username] = p
	passwords[a.Username] = a.Password
	return nil
}

func configuredPrincipal(a SubmissionAccount, relay receiptstore.ID) (principal, error) {
	p := principal{org: a.Organization, relay: relay, senders: map[string]bool{}, domains: map[string]bool{}}
	for _, address := range a.AuthorizedSenders {
		key, _, err := addressKey(address)
		if err != nil {
			return p, err
		}
		p.senders[key] = true
	}
	for _, domain := range a.AuthorizedDomains {
		domain = strings.ToLower(domain)
		if !domainValid(domain) {
			return p, fmt.Errorf("smtpserver: invalid authorized domain")
		}
		p.domains[domain] = true
	}
	if len(p.senders)+len(p.domains) == 0 {
		return p, fmt.Errorf("smtpserver: explicit sender authorization required")
	}
	return p, nil
}

func (s *session) authenticatePlain(identity, user, password string) error {
	if identity != "" && identity != user {
		return smtp.ErrAuthFailed
	}
	p, configured := s.server.accounts[user]
	if provider := s.server.config.Submission.Credentials; provider != nil {
		var err error
		p, err = s.authenticateLive(provider, user, password)
		if err != nil {
			return err
		}
	} else if !configured || !s.server.passwords.Verify(user, password) {
		return smtp.ErrAuthFailed
	}
	s.mu.Lock()
	s.account = &p
	s.mu.Unlock()
	return nil
}

func (s *session) authenticateLive(provider SubmissionCredentials, user, password string) (principal, error) {
	ctx, cancel := context.WithTimeout(s.server.ctx, 10*time.Second)
	defer cancel()
	live, err := provider.AuthenticateSubmission(ctx, user, password)
	if errors.Is(err, maildirectory.ErrNotFound) {
		return principal{}, smtp.ErrAuthFailed
	}
	if err != nil {
		return principal{}, temporary()
	}
	if live.Username != user || live.Organization == (receiptstore.ID{}) || live.Version < 1 {
		return principal{}, smtp.ErrAuthFailed
	}
	return s.livePrincipal(live)
}

func (s *session) livePrincipal(live maildirectory.SubmissionPrincipal) (principal, error) {
	senders, domains, err := receiptstore.NormalizeSubmissionScopes(live.AuthorizedSenders, live.AuthorizedDomains)
	if err != nil {
		return principal{}, smtp.ErrAuthFailed
	}
	relay := s.server.config.Submission.DefaultRelay
	if override, ok := s.server.config.Submission.OrganizationRelays[live.Organization]; ok {
		relay = override
	}
	p := principal{org: live.Organization, relay: relay, senders: map[string]bool{}, domains: map[string]bool{}}
	for _, value := range senders {
		p.senders[value] = true
	}
	for _, value := range domains {
		p.domains[value] = true
	}
	// Freeze this generation and policy; later edits invalidate this session.
	live.AuthorizedSenders = append([]string(nil), senders...)
	live.AuthorizedDomains = append([]string(nil), domains...)
	p.credential = &live
	return p, nil
}

func validSubmissionAuthority(transport string, config SubmissionConfig) bool {
	return transport == transportLoopback || (config.SenderAuthority != nil && config.Credentials != nil)
}
