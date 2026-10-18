//go:build linux

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/cotocisternas/fry/internal/dbschema"
	"github.com/cotocisternas/fry/internal/imapmigrate"
	"github.com/cotocisternas/fry/internal/imapsource"
	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/replicaconfig"
	"github.com/cotocisternas/fry/internal/telemetry"

	"github.com/jackc/pgx/v5/pgxpool"
)

type (
	migrationOptions struct {
		path, action, freezePath string
		generation               int64
	}
	migrationTarget struct{ organization, job, inbox receiptstore.ID }
	migrationOutput struct {
		Progress receiptstore.MigrationProgress
		Run      *imapmigrate.Result `json:",omitempty"`
	}
)

func parseMigrationOptions() (options migrationOptions, result error) {
	flag.StringVar(&options.path, "config", "", "operator migration JSON configuration")
	flag.StringVar(&options.action, "action", "status", "begin, begin-final, resume, status, or activate")
	flag.StringVar(&options.freezePath, "freeze", "", "final-pass barrier attestation JSON file")
	flag.Int64Var(&options.generation, "generation", 0, "expected generation, required for activation")
	flag.Parse()
	if options.path == "" {
		return options, fmt.Errorf("-config is required")
	}
	switch options.action {
	case "begin", "begin-final", "resume", "status", "activate":
	default:
		return options, fmt.Errorf("invalid action")
	}
	if (options.action == "begin-final") != (options.freezePath != "") {
		return options, fmt.Errorf("-freeze is required only with begin-final")
	}
	if options.action == "activate" && options.generation < 1 {
		return options, fmt.Errorf("activation requires -generation from the sealed final pass")
	}
	return options, nil
}

func (cfg config) target() (target migrationTarget, err error) {
	if cfg.TimeoutSeconds < 1 || cfg.TimeoutSeconds > 1800 {
		return target, fmt.Errorf("timeout_seconds must be between 1 and 1800")
	}
	if target.organization, err = replicaconfig.ParseID(cfg.Organization); err != nil {
		return target, err
	}
	if target.job, err = replicaconfig.ParseID(cfg.Job); err != nil {
		return target, err
	}
	target.inbox, err = replicaconfig.ParseID(cfg.Inbox)
	return target, err
}

func shutdownTelemetry(monitor *telemetry.Runtime) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if monitor.Shutdown(ctx) != nil {
		log.Print("telemetry shutdown incomplete")
	}
}

func openMigrationPool(ctx context.Context, dsn string, monitor *telemetry.Runtime) (*pgxpool.Pool, error) {
	dbc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid database configuration")
	}
	dbc.MaxConns = 16
	if err = monitor.ConfigurePool(dbc); err != nil {
		return nil, err
	}
	if err = dbschema.ConfigurePool(dbc); err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, dbc)
	if err != nil {
		return nil, err
	}
	return pool, nil
}

func executeMigration(
	ctx context.Context,
	pool *pgxpool.Pool,
	cfg config,
	options migrationOptions,
	target migrationTarget,
) error {
	store, closeStore, err := cfg.Open(ctx, pool)
	if err != nil {
		return err
	}
	defer func() {
		if err := closeStore(); err != nil {
			log.Print("migration storage cleanup incomplete; inspect durable progress before retrying")
		}
	}()

	output, err := performMigration(ctx, store, cfg, options, target)
	if err != nil {
		return err
	}
	output.Progress, err = store.MigrationProgress(ctx, target.organization, target.job)
	if err != nil {
		return err
	}
	if output.Progress.Inbox != target.inbox {
		return receiptstore.ErrConflict
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(output); err != nil {
		return fmt.Errorf("migration action completed but output failed; inspect durable progress before retrying")
	}
	return nil
}

func performMigration(
	ctx context.Context,
	store *receiptstore.Store,
	cfg config,
	options migrationOptions,
	target migrationTarget,
) (output migrationOutput, err error) {
	switch options.action {
	case "activate":
		err = activateMigration(ctx, store, options, target)
	case "status":
	default:
		var result imapmigrate.Result
		result, err = runMigrationImport(ctx, store, cfg, options, target)
		if err == nil {
			output.Run = &result
		}
	}
	return output, err
}

func activateMigration(
	ctx context.Context,
	store *receiptstore.Store,
	options migrationOptions,
	target migrationTarget,
) error {
	pass, e := store.ResumeMigration(ctx, target.organization, target.job)
	if e != nil {
		// Successful activation remains idempotent even after the final deadline.
		progress, statusErr := store.MigrationProgress(ctx, target.organization, target.job)
		if statusErr != nil || progress.Phase != "complete" || progress.Generation != options.generation ||
			progress.Inbox != target.inbox {
			return e
		}
	} else {
		if pass.Generation != options.generation || pass.Inbox != target.inbox {
			return receiptstore.ErrStale
		}
		if err := pass.Activate(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (cfg config) migrationWorker(store *receiptstore.Store, target migrationTarget) (*imapmigrate.Migrator, error) {
	password := os.Getenv(cfg.Source.PasswordEnvironment)
	if password == "" {
		return nil, fmt.Errorf("source password environment variable is required")
	}
	worker, err := imapmigrate.New(
		store,
		imapmigrate.Config{
			Organization: target.organization,
			Job:          target.job,
			Inbox:        target.inbox,
			Source: imapsource.Config{
				Address:           cfg.Source.Address,
				Username:          cfg.Source.Username,
				Password:          password,
				LoopbackPlaintext: cfg.Source.LoopbackPlaintext,
				OmitSubscriptions: cfg.Source.OmitSubscriptions,
				MaxMessageBytes:   cfg.MaxMessageBytes,
			},
			MaxMessages: cfg.MaxMessages,
			MaxWindows:  cfg.MaxWindows,
			Timeout:     time.Duration(cfg.TimeoutSeconds) * time.Second,
		},
	)
	if err != nil {
		return nil, err
	}
	return worker, nil
}

func prepareMigrationPass(
	ctx context.Context,
	worker *imapmigrate.Migrator,
	options migrationOptions,
) (*receiptstore.MigrationPass, error) {
	var err error
	var pass *receiptstore.MigrationPass
	if options.action == "resume" {
		pass, err = worker.Resume(ctx)
	} else {
		var freeze *receiptstore.MigrationFreeze
		if options.action == "begin-final" {
			freeze = &receiptstore.MigrationFreeze{}
			if err = readJSON(options.freezePath, freeze); err != nil {
				return nil, err
			}
		}
		pass, err = worker.Begin(ctx, freeze)
	}
	if err != nil {
		return nil, err
	}
	return pass, nil
}

func runMigrationImport(
	ctx context.Context,
	store *receiptstore.Store,
	cfg config,
	options migrationOptions,
	target migrationTarget,
) (imapmigrate.Result, error) {
	worker, err := cfg.migrationWorker(store, target)
	if err != nil {
		return imapmigrate.Result{}, err
	}
	pass, err := prepareMigrationPass(ctx, worker, options)
	if err != nil {
		return imapmigrate.Result{}, err
	}
	// Any obsolete attempt still needs actual member cancellation, not just a
	// generation fence. Failures retain cleanup obligations and stop this run.
	if _, err = store.ReapMigrationAttempts(ctx, target.organization, target.job, 128); err != nil {
		return imapmigrate.Result{}, err
	}
	result, err := worker.Run(ctx, pass)
	if err != nil {
		return imapmigrate.Result{}, err
	}
	return result, nil
}
