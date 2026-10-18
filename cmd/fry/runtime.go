//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/cotocisternas/fry/internal/dbschema"
	"github.com/cotocisternas/fry/internal/imapserver"
	"github.com/cotocisternas/fry/internal/installation"
	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/smtpserver"
	"github.com/cotocisternas/fry/internal/telemetry"

	"github.com/jackc/pgx/v5/pgxpool"
)

type startupOptions struct {
	path                                    string
	initialize, initializeOnly, checkSchema bool
}

func parseStartupOptions() (startupOptions, error) {
	var options startupOptions
	flag.StringVar(&options.path, "config", "", "local JSON configuration file")
	flag.BoolVar(&options.initialize, "init", false, "initialize a fresh, dedicated PostgreSQL schema")
	flag.BoolVar(
		&options.initializeOnly,
		"init-only",
		false,
		"initialize fresh receipt and administration schemas, then exit",
	)
	flag.BoolVar(
		&options.checkSchema,
		"check-schema",
		false,
		"check complete database schema compatibility without opening mail storage",
	)
	flag.Parse()
	if options.checkSchema && (options.initialize || options.initializeOnly) {
		return options, fmt.Errorf("-check-schema cannot be combined with initialization")
	}
	if options.path == "" {
		return options, fmt.Errorf("-config is required (see examples/local-smtp.json)")
	}
	return options, nil
}

func readConfig(path string) (cfg config, address *net.TCPAddr, result error) {
	// #nosec G304 -- Operator-selected -config path, never remote request input. Owner: FRY maintainers; review by
	// 2027-01-18.
	file, err := os.Open(path)
	if err != nil {
		return cfg, nil, err
	}
	defer func() {
		if file != nil {
			if e := file.Close(); e != nil {
				log.Print("configuration close incomplete")
			}
		}
	}()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&cfg); err != nil {
		return cfg, nil, err
	}
	if err = file.Close(); err != nil {
		return cfg, nil, fmt.Errorf("configuration close failed")
	}
	file = nil
	if err = cfg.Validate(); err != nil {
		return cfg, nil, err
	}
	if err = cfg.prepareListeners(); err != nil {
		return cfg, nil, err
	}
	address, err = net.ResolveTCPAddr("tcp", cfg.Listen)
	if err != nil {
		return cfg, nil, err
	}

	return cfg, address, nil
}

func shutdownTelemetry(monitor *telemetry.Runtime) {
	shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	if monitor.Shutdown(shutdown) != nil {
		log.Print("telemetry shutdown incomplete")
	}
}

func openCommandPool(ctx context.Context, dsn string, monitor *telemetry.Runtime) (*pgxpool.Pool, error) {
	dbConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid FRY_DATABASE_URL")
	}
	dbConfig.MaxConns = 16
	if err = monitor.ConfigurePool(dbConfig); err != nil {
		return nil, err
	}
	if err = dbschema.ConfigurePool(dbConfig); err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, dbConfig)
	if err != nil {
		return nil, fmt.Errorf("database initialization failed: %w", err)
	}
	return pool, nil
}

func prepareSchema(ctx context.Context, pool *pgxpool.Pool, options startupOptions) (bool, error) {
	var err error
	if options.initialize || options.initializeOnly {
		if err = installation.Initialize(ctx, pool); err != nil {
			return false, fmt.Errorf("fresh schema initialization: %w", err)
		}
	}
	if options.initializeOnly {
		return true, nil
	}
	if err = installation.Check(ctx, pool); err != nil {
		return false, fmt.Errorf("schema compatibility: %w", err)
	}
	if options.checkSchema {
		log.Print("FRY schema compatible")
		return true, nil
	}
	return false, nil
}

func serveCoordinator(
	ctx context.Context,
	pool *pgxpool.Pool,
	cfg config,
	monitor *telemetry.Runtime,
	address *net.TCPAddr,
) (runErr error) {
	store, closeStore, err := cfg.Open(ctx, pool)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, closeStore()) }()
	if err = requireObjectConfiguration(ctx, pool, cfg.Lifecycle); err != nil {
		return err
	}

	stopLifecycle, err := startLifecycle(ctx, store, cfg.Lifecycle, cfg.Storage)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, stopLifecycle()) }()
	return serveMail(ctx, pool, store, cfg, monitor, address)
}

func serveMail(
	ctx context.Context,
	pool *pgxpool.Pool,
	store *receiptstore.Store,
	cfg config,
	monitor *telemetry.Runtime,
	address *net.TCPAddr,
) error {
	smtpConfig := smtpserver.Config{
		Telemetry:        monitor,
		Transport:        cfg.Transport,
		TLSConfig:        cfg.tls,
		HandshakeTimeout: cfg.handshakeTimeout(),
		Domain:           cfg.Domain,
		MaxConnections:   cfg.MaxConnections,
		MaxInFlight:      cfg.MaxInFlight,
		MaxRecipients:    100,
		MaxMessageBytes:  cfg.MaxMessageBytes,
		IdleTimeout:      time.Minute,
		IngestTimeout:    5 * time.Minute,
	}
	directory, err := prepareDirectory(ctx, pool, store, cfg)
	if err != nil {
		return err
	}
	smtpConfig.Directory = directory

	server, err := smtpserver.New(store, smtpConfig)
	if err != nil {
		return err
	}
	defer func() {
		shutdown, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer done()
		if err := server.Shutdown(shutdown); err != nil {
			log.Print("SMTP shutdown required connection cancellation")
		}
	}()
	stopCollectors, err := startCollectors(ctx, store, cfg.Collections, cfg.MaxMessageBytes)
	if err != nil {
		return err
	}
	defer stopCollectors()
	stopWorkers := startMailWorkers(ctx, store)
	defer stopWorkers()
	return serveProtocols(ctx, pool, store, directory, server, cfg, monitor, address)
}

func startMailWorkers(parent context.Context, store *receiptstore.Store) func() {
	ctx, cancel := context.WithCancel(parent)
	publicationDone, closureDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(publicationDone); runMailboxPublication(ctx, store) }()
	go func() { defer close(closureDone); runMemberClosures(ctx, store) }()
	return func() { cancel(); <-closureDone; <-publicationDone }
}

func runMailboxPublication(ctx context.Context, store *receiptstore.Store) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		callCtx, callCancel := context.WithTimeout(ctx, 30*time.Second)
		n, e := store.PublishMailboxes(callCtx, 128)
		callCancel()
		if e != nil && ctx.Err() == nil {
			log.Print("Mailbox publication will retry after a storage error")
		}
		if e == nil && n == 128 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runMemberClosures(ctx context.Context, store *receiptstore.Store) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if _, e := store.ReconcileMemberClosures(ctx, 128); e != nil && ctx.Err() == nil {
			log.Print("Member closure reconciliation will retry after a storage error")
		}
	}
}

func serveProtocols(
	ctx context.Context,
	pool *pgxpool.Pool,
	store *receiptstore.Store,
	directory *maildirectory.Directory,
	server *smtpserver.Server,
	cfg config,
	monitor *telemetry.Runtime,
	address *net.TCPAddr,
) error {
	var err error
	protocolErrors := make(chan error, 4)
	stopOutbound, err := startOutbound(ctx, store, directory, cfg, protocolErrors)
	if err != nil {
		return err
	}
	defer stopOutbound()
	stopIMAP, err := startIMAP(store, directory, cfg, monitor, protocolErrors)
	if err != nil {
		return err
	}
	defer stopIMAP()
	stopAdmin, err := startAdmin(ctx, pool, store, cfg.Admin, protocolErrors)
	if err != nil {
		return err
	}
	defer stopAdmin()
	listener, err := net.ListenTCP("tcp", address)
	if err != nil {
		return err
	}
	defer closeCommandListener(listener)
	go func() { protocolErrors <- server.Serve(listener) }()
	log.Printf("FRY SMTP listening on %s", listener.Addr())
	select {
	case err = <-protocolErrors:
	case <-ctx.Done():
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer shutdownCancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	if err != nil {
		return err
	}
	if shutdownErr != nil {
		log.Print("SMTP shutdown required connection cancellation")
	}
	return nil
}

func startIMAP(
	store *receiptstore.Store,
	directory *maildirectory.Directory,
	cfg config,
	monitor *telemetry.Runtime,
	protocolErrors chan<- error,
) (func(), error) {
	if cfg.IMAP == nil {
		return func() {}, nil
	}
	imapAddress, e := net.ResolveTCPAddr("tcp", cfg.IMAP.Listen)
	if e != nil {
		return nil, e
	}
	imap, e := imapserver.NewWithDirectoryOptions(
		store,
		directory,
		imapserver.Options{
			Telemetry:        monitor,
			Transport:        cfg.IMAP.Transport,
			TLS:              cfg.IMAP.tls,
			HandshakeTimeout: cfg.IMAP.handshakeTimeout(),
			MaxConnections:   cfg.MaxConnections,
		},
	)
	if e != nil {
		return nil, e
	}
	stopServer := func() {
		if err := imap.Close(); err != nil {
			log.Print("IMAP shutdown incomplete")
		}
	}
	if cfg.MaxMessageBytes < 1 || cfg.MaxMessageBytes > 256<<20 {
		stopServer()
		return nil, fmt.Errorf("invalid append limit")
	}
	if e = imap.SetAppendLimit(uint32(cfg.MaxMessageBytes)); e != nil {
		stopServer()
		return nil, e
	}
	imapListener, e := net.ListenTCP("tcp", imapAddress)
	if e != nil {
		stopServer()
		return nil, e
	}
	stopListener := func() { closeCommandListener(imapListener) }
	go func() { protocolErrors <- imap.Serve(imapListener) }()
	log.Printf("FRY IMAP listening on %s", imapListener.Addr())
	return func() { stopListener(); stopServer() }, nil
}

func closeCommandListener(listener net.Listener) {
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Print("listener close incomplete")
	}
}
