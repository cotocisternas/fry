//go:build linux

// Command fry-migrate runs bounded read-only history imports into staging.
// Direct database/private-replica credentials make this an operator-only tool.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cotocisternas/fry/internal/installation"
	"github.com/cotocisternas/fry/internal/replicaconfig"
	"github.com/cotocisternas/fry/internal/telemetry"
)

type config struct {
	Telemetry telemetry.Config `json:"telemetry"`
	replicaconfig.Storage
	Organization   string `json:"organization"`
	Inbox          string `json:"inbox"`
	Job            string `json:"job"`
	MaxMessages    int    `json:"max_messages"`
	MaxWindows     int    `json:"max_windows"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Source         struct {
		Address             string `json:"address"`
		Username            string `json:"username"`
		PasswordEnvironment string `json:"password_environment"`
		LoopbackPlaintext   bool   `json:"loopback_plaintext"`
		OmitSubscriptions   bool   `json:"omit_subscriptions"`
	} `json:"source"`
}

func readJSON(path string, target any) (result error) {
	// Operator-selected configuration paths are intentional, never remote input.
	// G304: operator file selection; owner: FRY maintainers; expires: 2027-01-18.
	//nolint:gosec // Reviewed call; rationale above.
	f, err := os.Open(
		path,
	)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, f.Close()) }()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Size() > 1<<20 {
		return fmt.Errorf("configuration must be a regular file at most 1 MiB")
	}
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err = decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("configuration must contain exactly one JSON value")
	}
	return nil
}

func run() error {
	options, err := parseMigrationOptions()
	if err != nil {
		return err
	}
	var cfg config
	if err = readJSON(options.path, &cfg); err != nil {
		return err
	}
	target, err := cfg.target()
	if err != nil {
		return err
	}
	dsn := os.Getenv("FRY_DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("FRY_DATABASE_URL is required")
	}
	parent, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, stop := context.WithTimeout(parent, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer stop()
	monitor, err := telemetry.New(ctx, "fry-migrate", cfg.Telemetry)
	if err != nil {
		return fmt.Errorf("telemetry configuration: %w", err)
	}
	defer shutdownTelemetry(monitor)
	ctx = telemetry.WithRuntime(ctx, monitor)
	pool, err := openMigrationPool(ctx, dsn, monitor)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err = installation.Check(ctx, pool); err != nil {
		return fmt.Errorf("schema compatibility: %w", err)
	}
	return executeMigration(ctx, pool, cfg, options, target)
}

func main() {
	if err := run(); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, "fry-migrate:", err); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}
