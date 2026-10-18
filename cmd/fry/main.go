//go:build linux

// Command fry runs the local SMTP/storage prototype with explicit configuration.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/replicaconfig"
	"github.com/cotocisternas/fry/internal/telemetry"
)

type config struct {
	Telemetry telemetry.Config `json:"telemetry"`
	replicaconfig.Storage
	transportConfig
	Lifecycle      *lifecycleConfig   `json:"storage_lifecycle"`
	Admin          *adminConfig       `json:"admin"`
	Outbound       *outboundConfig    `json:"outbound"`
	Collections    []collectionConfig `json:"collections"`
	Domain         string             `json:"domain"`
	Listen         string             `json:"listen"`
	MaxConnections int                `json:"max_connections"`
	Routes         []struct {
		Address      string   `json:"address"`
		Organization string   `json:"organization"`
		Inboxes      []string `json:"inboxes"`
	} `json:"routes"`
	IMAP *struct {
		transportConfig
		Listen   string `json:"listen"`
		Accounts []struct {
			Username            string `json:"username"`
			PasswordEnvironment string `json:"password_environment"`
			Organization        string `json:"organization"`
			Inbox               string `json:"inbox"`
		} `json:"accounts"`
	} `json:"imap"`
}

func parseID(raw string) (receiptstore.ID, error) { return replicaconfig.ParseID(raw) }
func run() error {
	options, err := parseStartupOptions()
	if err != nil {
		return err
	}
	cfg, address, err := readConfig(options.path)
	if err != nil {
		return err
	}
	dsn := os.Getenv("FRY_DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("FRY_DATABASE_URL is required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	monitor, err := telemetry.New(ctx, "fry", cfg.Telemetry)
	if err != nil {
		return fmt.Errorf("telemetry configuration: %w", err)
	}
	defer shutdownTelemetry(monitor)
	ctx = telemetry.WithRuntime(ctx, monitor)
	pool, err := openCommandPool(ctx, dsn, monitor)
	if err != nil {
		return err
	}
	defer pool.Close()
	done, err := prepareSchema(ctx, pool, options)
	if err != nil || done {
		return err
	}
	return serveCoordinator(ctx, pool, cfg, monitor, address)
}

func main() {
	if err := run(); err != nil {
		log.Print("command failed; check configuration and service health")
		os.Exit(1)
	}
}
