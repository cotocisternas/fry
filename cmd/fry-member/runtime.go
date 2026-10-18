//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/cotocisternas/fry/internal/dbschema"
	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/replicaconfig"
	"github.com/cotocisternas/fry/internal/telemetry"

	"github.com/jackc/pgx/v5/pgxpool"
)

type memberStartup struct {
	config  config
	id      receiptstore.ID
	storage hotstore.Options
	address *net.TCPAddr
	tls     *tls.Config
	keys    [][32]byte
}

func readMemberConfig(path string) (cfg config, result error) {
	// #nosec G304 -- Operator-selected -config path, never remote request input. Owner: FRY maintainers; review by
	// 2027-01-18.
	file, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer func() {
		if file != nil {
			if e := file.Close(); e != nil {
				log.Print("configuration close incomplete")
			}
		}
	}()
	d := json.NewDecoder(io.LimitReader(file, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&cfg); err != nil {
		return cfg, err
	}
	if d.Decode(new(any)) != io.EOF {
		return cfg, fmt.Errorf("expected one JSON configuration")
	}
	if err = file.Close(); err != nil {
		return cfg, fmt.Errorf("configuration close failed")
	}
	file = nil
	return cfg, nil
}

func prepareMember(cfg config) (result memberStartup, err error) {
	raw, err := hex.DecodeString(cfg.ID)
	var id receiptstore.ID
	if err != nil || len(raw) != len(id) {
		return result, fmt.Errorf("invalid member ID")
	}
	copy(id[:], raw)
	if err := cfg.validateBounds(id); err != nil {
		return result, err
	}
	storageOptions := hotstore.Options{
		MaxMessageBytes:      cfg.MaxMessageBytes,
		MaxBytes:             cfg.CapacityBytes,
		MaxConcurrentAppends: cfg.MaxInFlight,
		MaxRecords:           cfg.SegmentMaxRecords,
		MaxFileBytes:         cfg.SegmentMaxBytes,
	}
	if err := receiptstore.ValidateMemberOptions(storageOptions); err != nil {
		return result, fmt.Errorf("member storage: %w", err)
	}
	address, err := net.ResolveTCPAddr("tcp", cfg.Listen)
	if err != nil || cfg.Listen == "" {
		return result, fmt.Errorf("invalid listen address")
	}
	tls, err := cfg.TLS.Load()
	if err != nil {
		return result, err
	}
	keys := make([][32]byte, len(cfg.CoordinatorKeys))
	for i, key := range cfg.CoordinatorKeys {
		keys[i], err = replicaconfig.Key(key)
		if err != nil {
			return result, err
		}
	}
	return memberStartup{cfg, id, storageOptions, address, tls, keys}, nil
}

func (cfg config) validateBounds(id receiptstore.ID) error {
	if id == (receiptstore.ID{}) || cfg.Domain == "" || cfg.DataDirectory == "" || !cfg.validLimits() {
		return fmt.Errorf("invalid bounded member configuration")
	}
	return nil
}

func shutdownTelemetry(monitor *telemetry.Runtime) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if monitor.Shutdown(ctx) != nil {
		log.Print("telemetry shutdown incomplete")
	}
}

func openMemberPool(ctx context.Context, dsn string, monitor *telemetry.Runtime) (*pgxpool.Pool, error) {
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
		return nil, fmt.Errorf("member database initialization failed")
	}
	return pool, nil
}

func serveMember(ctx context.Context, pool *pgxpool.Pool, startup memberStartup) (result error) {
	directory, err := replicaconfig.Directory(startup.config.DataDirectory)
	if err != nil {
		return err
	}
	member, err := receiptstore.OpenLocalMember(
		ctx,
		pool,
		startup.id,
		startup.config.Domain,
		directory,
		startup.storage,
	)
	if err != nil {
		return err
	}
	server, err := receiptstore.NewMemberServer(
		ctx,
		member,
		receiptstore.MemberServerOptions{
			TLS:             startup.tls,
			CoordinatorKeys: startup.keys,
			MaxRequests:     startup.config.MaxRequests,
			Timeout:         time.Duration(startup.config.TimeoutSeconds) * time.Second,
		},
	)
	if err != nil {
		if closeErr := member.Close(); closeErr != nil {
			log.Print("member cleanup left reconciliation obligations")
		}
		return err
	}
	defer func() {
		shutdown, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer stop()
		if e := server.Close(shutdown); e != nil {
			log.Print("Member shutdown left reconciliation obligations")
			result = errors.Join(result, e)
		}
	}()
	listener, err := net.ListenTCP("tcp", startup.address)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	log.Printf("FRY member %s listening with mutual TLS on %s", startup.id.String(), listener.Addr())
	select {
	case <-ctx.Done():
		return nil
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (cfg config) validLimits() bool {
	return cfg.MaxMessageBytes >= 1 && cfg.MaxMessageBytes <= 256<<20 && cfg.MaxRequests >= 1 &&
		cfg.MaxRequests <= 128 &&
		cfg.TimeoutSeconds >= 1 &&
		cfg.TimeoutSeconds <= 300 &&
		len(cfg.CoordinatorKeys) >= 1 &&
		len(cfg.CoordinatorKeys) <= 256
}
