//go:build linux

// Command fry-member serves one explicitly configured, mutually authenticated
// storage member. Schema initialization belongs to fry -init-only.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/cotocisternas/fry/internal/installation"
	"github.com/cotocisternas/fry/internal/replicaconfig"
	"github.com/cotocisternas/fry/internal/telemetry"
)

type config struct {
	Telemetry         telemetry.Config  `json:"telemetry"`
	ID                string            `json:"id"`
	Domain            string            `json:"domain"`
	Listen            string            `json:"listen"`
	DataDirectory     string            `json:"data_directory"`
	MaxMessageBytes   int64             `json:"max_message_bytes"`
	CapacityBytes     int64             `json:"capacity_bytes"`
	SegmentMaxRecords int64             `json:"segment_max_records,omitempty"`
	SegmentMaxBytes   int64             `json:"segment_max_bytes,omitempty"`
	MaxInFlight       int               `json:"max_in_flight"`
	MaxRequests       int               `json:"max_requests"`
	TimeoutSeconds    int               `json:"timeout_seconds"`
	TLS               replicaconfig.TLS `json:"tls"`
	CoordinatorKeys   []string          `json:"coordinator_keys"`
}

func run() error {
	path := flag.String("config", "", "member JSON configuration file")
	flag.Parse()
	if *path == "" {
		return fmt.Errorf("-config is required")
	}
	cfg, err := readMemberConfig(*path)
	if err != nil {
		return err
	}
	startup, err := prepareMember(cfg)
	if err != nil {
		return err
	}
	dsn := os.Getenv("FRY_DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("FRY_DATABASE_URL is required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	monitor, err := telemetry.New(ctx, "fry-member", cfg.Telemetry)
	if err != nil {
		return fmt.Errorf("telemetry configuration: %w", err)
	}
	defer shutdownTelemetry(monitor)
	ctx = telemetry.WithRuntime(ctx, monitor)
	pool, err := openMemberPool(ctx, dsn, monitor)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err = installation.Check(ctx, pool); err != nil {
		return fmt.Errorf("schema compatibility: %w", err)
	}
	return serveMember(ctx, pool, startup)
}

func main() {
	if err := run(); err != nil {
		log.Print("command failed; check configuration and service health")
		os.Exit(1)
	}
}
