//go:build linux

package main

import (
	"context"
	"crypto/x509"
	stderrors "errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/outbound"
	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/smtpserver"
	"github.com/cotocisternas/fry/internal/telemetry"
)

type outboundConfig struct {
	DefaultRelay     string `json:"default_relay"`
	RetryHours       int    `json:"retry_hours"`
	NoticeRetryHours int    `json:"notice_retry_hours"`
	Relays           []struct {
		ID                  string `json:"id"`
		Address             string `json:"address"`
		Transport           string `json:"transport"`
		Username            string `json:"username"`
		PasswordEnvironment string `json:"password_environment"`
		CABundle            string `json:"ca_bundle"`
	} `json:"relays"`
	OrganizationRelays map[string]string `json:"organization_relays"`
	Submission         *struct {
		transportConfig
		Listen   string `json:"listen"`
		Accounts []struct {
			Username            string   `json:"username"`
			PasswordEnvironment string   `json:"password_environment"`
			Organization        string   `json:"organization"`
			AuthorizedSenders   []string `json:"authorized_senders"`
			AuthorizedDomains   []string `json:"authorized_domains"`
		} `json:"accounts"`
	} `json:"submission"`
}

func startOutbound(
	parent context.Context,
	store *receiptstore.Store,
	directory *maildirectory.Directory,
	cfg config,
	errors chan<- error,
) (func(), error) {
	if cfg.Outbound == nil {
		if err := requireNoPendingOutbound(parent, store); err != nil {
			return nil, err
		}
		return func() {}, nil
	}
	c := cfg.Outbound
	if c.RetryHours < 1 || c.RetryHours > 24*30 || c.NoticeRetryHours < 1 || c.NoticeRetryHours > 24*30 {
		return nil, fmt.Errorf("explicit outbound and notice retry hours between 1 and 720 are required")
	}
	ctx, cancel := context.WithCancel(parent)
	runtime := &outboundRuntime{cancel: cancel}
	worker, err := prepareOutbound(ctx, store, directory, cfg, runtime, errors)
	if err != nil {
		runtime.stop(parent)
		return nil, err
	}
	runtime.wg.Add(1)
	go func() { defer runtime.wg.Done(); runOutbound(ctx, store, worker, cfg) }()
	return func() { runtime.stop(parent) }, nil
}

type outboundRuntime struct {
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	submission *smtpserver.Server
	listener   net.Listener
}

func (runtime *outboundRuntime) stop(parent context.Context) {
	runtime.cancel()
	if runtime.listener != nil {
		if err := runtime.listener.Close(); err != nil && !stderrors.Is(err, net.ErrClosed) {
			log.Print("submission listener close incomplete")
		}
	}
	if runtime.submission != nil {
		deadline, done := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
		if err := runtime.submission.Shutdown(deadline); err != nil {
			log.Print("submission shutdown required connection cancellation")
		}
		done()
	}
	runtime.wg.Wait()
}

func requireNoPendingOutbound(parent context.Context, store *receiptstore.Store) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	pending, err := store.HasPendingOutbound(ctx)
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("outbound configuration is required while submissions or failure notices remain pending")
	}
	return nil
}

func configuredRelays(c *outboundConfig) ([]outbound.Relay, map[receiptstore.ID]bool, error) {
	var relays []outbound.Relay
	known := map[receiptstore.ID]bool{}
	for _, item := range c.Relays {
		id, err := parseID(item.ID)
		if err != nil {
			return nil, nil, err
		}
		relay := outbound.Relay{
			ID:        id,
			Address:   item.Address,
			Transport: item.Transport,
			Username:  item.Username,
			Password:  os.Getenv(item.PasswordEnvironment),
		}
		if item.CABundle != "" {
			pem, err := os.ReadFile(item.CABundle)
			if err != nil {
				return nil, nil, err
			}
			relay.Roots = x509.NewCertPool()
			if !relay.Roots.AppendCertsFromPEM(pem) {
				return nil, nil, fmt.Errorf("relay CA bundle has no certificates")
			}
		}
		relays = append(relays, relay)
		known[id] = true
	}
	return relays, known, nil
}

func submissionRouting(c *outboundConfig, known map[receiptstore.ID]bool) (*smtpserver.SubmissionConfig, error) {
	defaultRelay, err := parseID(c.DefaultRelay)
	if err != nil || !known[defaultRelay] {
		return nil, fmt.Errorf("default relay must reference a configured relay")
	}
	submissionConfig := &smtpserver.SubmissionConfig{
		DefaultRelay:       defaultRelay,
		RetryWindow:        time.Duration(c.RetryHours) * time.Hour,
		OrganizationRelays: map[receiptstore.ID]receiptstore.ID{},
	}
	for rawOrg, rawRelay := range c.OrganizationRelays {
		org, e := parseID(rawOrg)
		if e != nil {
			return nil, e
		}
		relay, e := parseID(rawRelay)
		if e != nil || !known[relay] {
			return nil, fmt.Errorf("organization override must reference a configured relay")
		}
		submissionConfig.OrganizationRelays[org] = relay
	}
	return submissionConfig, nil
}

func prepareOutbound(
	ctx context.Context,
	store *receiptstore.Store,
	directory *maildirectory.Directory,
	cfg config,
	runtime *outboundRuntime,
	errors chan<- error,
) (*outbound.Worker, error) {
	relays, known, err := configuredRelays(cfg.Outbound)
	if err != nil {
		return nil, err
	}
	// Constructor owns its bounded binding context; external API has no context; owner: FRY
	// maintainers; expires: 2027-01-18.
	//nolint:contextcheck // Reviewed call; rationale above.
	worker, err := outbound.New(
		store,
		cfg.Domain,
		relays,
	)
	if err != nil {
		return nil, err
	}
	submissionConfig, err := submissionRouting(cfg.Outbound, known)
	if err != nil {
		return nil, err
	}
	if cfg.Outbound.Submission != nil {
		if err = prepareSubmission(ctx, store, directory, cfg, submissionConfig, runtime, errors); err != nil {
			return nil, err
		}
	}
	return worker, nil
}

func prepareSubmission(
	ctx context.Context,
	store *receiptstore.Store,
	directory *maildirectory.Directory,
	cfg config,
	submissionConfig *smtpserver.SubmissionConfig,
	runtime *outboundRuntime,
	errors chan<- error,
) error {
	c := cfg.Outbound
	if directory == nil {
		return fmt.Errorf("live submission credential directory required")
	}
	submissionConfig.Credentials = directory
	if c.Submission.Transport != "loopback" {
		submissionConfig.SenderAuthority = store
	}
	address, e := net.ResolveTCPAddr("tcp", c.Submission.Listen)
	if e != nil {
		return e
	}
	if err := seedSubmissionAccounts(ctx, c, directory, submissionConfig); err != nil {
		return err
	}

	runtime.submission, e = smtpserver.New(
		store,
		smtpserver.Config{
			Telemetry:        telemetry.FromContext(ctx),
			Transport:        c.Submission.Transport,
			TLSConfig:        c.Submission.tls,
			HandshakeTimeout: c.Submission.handshakeTimeout(),
			Domain:           cfg.Domain,
			Submission:       submissionConfig,
			MaxConnections:   cfg.MaxConnections,
			MaxInFlight:      cfg.MaxInFlight,
			MaxRecipients:    100,
			MaxMessageBytes:  cfg.MaxMessageBytes,
			IdleTimeout:      time.Minute,
			IngestTimeout:    5 * time.Minute,
		},
	)
	if e != nil {
		return e
	}
	runtime.listener, e = net.ListenTCP("tcp", address)
	if e != nil {
		return e
	}
	go func() { errors <- runtime.submission.Serve(runtime.listener) }()
	log.Printf("FRY submission listening on %s", runtime.listener.Addr())
	return nil
}

func runOutbound(ctx context.Context, store *receiptstore.Store, worker *outbound.Worker, cfg config) {
	c := cfg.Outbound
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		notices, e := store.QueueFailureNotices(ctx, cfg.Domain, time.Duration(c.NoticeRetryHours)*time.Hour, 32)
		logOutboundFailure(ctx, e, "Outbound failure-notice queue will retry after a storage error")
		did, sendErr := worker.RunOnce(ctx)
		logOutboundFailure(ctx, sendErr, "Outbound handoff progress could not be recorded; lease recovery will retry")
		if e == nil && sendErr == nil && (did || notices > 0) {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func logOutboundFailure(ctx context.Context, err error, message string) {
	if err != nil && ctx.Err() == nil {
		log.Print(message)
	}
}

func seedSubmissionAccounts(
	ctx context.Context,
	c *outboundConfig,
	directory *maildirectory.Directory,
	submissionConfig *smtpserver.SubmissionConfig,
) error {
	var seedAccounts []maildirectory.SubmissionAccount
	var seedPasswords []string
	for _, a := range c.Submission.Accounts {
		org, e := parseID(a.Organization)
		if e != nil {
			return e
		}
		seedAccounts = append(
			seedAccounts,
			maildirectory.SubmissionAccount{
				Username:          a.Username,
				Organization:      org.String(),
				Enabled:           true,
				AuthorizedSenders: a.AuthorizedSenders,
				AuthorizedDomains: a.AuthorizedDomains,
			},
		)
		seedPasswords = append(seedPasswords, os.Getenv(a.PasswordEnvironment))
		submissionConfig.Accounts = append(
			submissionConfig.Accounts,
			smtpserver.SubmissionAccount{
				Username:          a.Username,
				Organization:      org,
				AuthorizedSenders: a.AuthorizedSenders,
				AuthorizedDomains: a.AuthorizedDomains,
			},
		)
	}
	return directory.SeedSubmission(ctx, seedAccounts, seedPasswords)
}
