//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/objectstore"
	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/replicaconfig"

	"github.com/jackc/pgx/v5/pgxpool"
)

type lifecycleConfig struct {
	Objects                *objectstore.S3Config `json:"objects"`
	ScratchDirectory       string                `json:"scratch_directory"`
	MaxPackBytes           int64                 `json:"max_pack_bytes"`
	MaxStagingBytes        int64                 `json:"max_staging_bytes"`
	PackLeaseSeconds       int                   `json:"pack_lease_seconds"`
	IntervalSeconds        int                   `json:"interval_seconds"`
	WorkTimeoutSeconds     int                   `json:"work_timeout_seconds"`
	MaxActions             int                   `json:"max_actions"`
	MaxReceipts            int                   `json:"max_receipts"`
	ReclaimHot             bool                  `json:"reclaim_hot"`
	RotateHot              bool                  `json:"rotate_hot"`
	ReclaimIntervalSeconds int                   `json:"reclaim_interval_seconds"`
}

type hotReclaimer interface {
	ReclaimHot(context.Context, receiptstore.ID) error
	ReclaimSealed(context.Context, receiptstore.ID) error
	RotateHotIfNeeded(context.Context, receiptstore.ID) error
}

// Each scheduled opportunity attempts one member. Alternate complete sweeps so
// an unchanged or blocked file cannot consume every turn of either file class.
// This cursor is process-local; it does not promise fairness across restarts.
type hotReclaimCursor struct {
	member, phase int
	rotate        bool
}

func (c *hotReclaimCursor) run(ctx context.Context, store hotReclaimer, members []receiptstore.ID) error {
	member, phase := members[c.member], c.phase
	c.member++
	if c.member == len(members) {
		c.member = 0
		classes := 2
		if c.rotate {
			classes = 3
		}
		c.phase = (c.phase + 1) % classes
	}
	switch phase {
	case 1:
		return store.ReclaimSealed(ctx, member)
	case 2:
		return store.RotateHotIfNeeded(ctx, member)
	default:
		return store.ReclaimHot(ctx, member)
	}
}

// startLifecycle wires bounded scheduling to the running coordinator. All
// publication/removal authority stays in receiptstore, not in this polling loop.
func startLifecycle(
	parent context.Context,
	store *receiptstore.Store,
	cfg *lifecycleConfig,
	storage replicaconfig.Storage,
) (func() error, error) {
	if cfg == nil {
		return func() error { return nil }, nil
	}
	if err := cfg.validateScheduling(); err != nil {
		return nil, err
	}
	var packer *receiptstore.Packer
	var err error
	if cfg.Objects != nil {
		if err = cfg.validatePacking(storage); err != nil {
			return nil, err
		}
		startup, stop := context.WithTimeout(parent, time.Duration(cfg.WorkTimeoutSeconds)*time.Second)
		defer stop()
		packer, err = cfg.openPacker(startup, store)
		if err != nil {
			return nil, err
		}
	}
	maintenance, members, err := prepareMaintenance(store, packer, cfg, storage)
	if err != nil {
		if packer != nil {
			err = errors.Join(err, packer.Close())
		}
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() { defer close(done); runLifecycle(ctx, store, maintenance, cfg, members) }()
	var once sync.Once
	var closeErr error
	return func() error {
		once.Do(func() {
			cancel()
			<-done
			if packer != nil {
				closeErr = packer.Close()
			}
		})
		return closeErr
	}, nil
}

// Once bound, configuration remains mandatory even if no live mailbox points
// at a pack: unpublished uploads and retirement markers retain obligations.
func requireObjectConfiguration(ctx context.Context, pool *pgxpool.Pool, cfg *lifecycleConfig) error {
	required, err := receiptstore.RequiresObjectStorage(ctx, pool)
	if err != nil {
		return err
	}
	if required && (cfg == nil || cfg.Objects == nil) {
		return fmt.Errorf(
			"bound object storage and historical pack obligations require " +
				"storage_lifecycle.objects configuration",
		)
	}
	return nil
}

func (cfg *lifecycleConfig) validateScheduling() error {
	if cfg.RotateHot && !cfg.ReclaimHot {
		return fmt.Errorf("automatic rotation requires hot reclamation")
	}
	if !cfg.validBatchSchedule() || !cfg.validReclaimSchedule() {
		return fmt.Errorf("invalid storage lifecycle scheduling bounds")
	}
	return nil
}

func (cfg *lifecycleConfig) validBatchSchedule() bool {
	return cfg.IntervalSeconds >= 1 && cfg.IntervalSeconds <= 3600 && cfg.WorkTimeoutSeconds >= 1 &&
		cfg.WorkTimeoutSeconds <= 300 &&
		cfg.MaxActions >= 1 &&
		cfg.MaxActions <= 5 &&
		cfg.MaxReceipts >= 1 &&
		cfg.MaxReceipts <= 1024
}

func (cfg *lifecycleConfig) validReclaimSchedule() bool {
	return !cfg.ReclaimHot || (cfg.ReclaimIntervalSeconds >= 1 && cfg.ReclaimIntervalSeconds <= 86400)
}

func (cfg *lifecycleConfig) validatePacking(storage replicaconfig.Storage) error {
	if cfg.PackLeaseSeconds < 1 || cfg.PackLeaseSeconds > 300 {
		return fmt.Errorf("pack lease must be between 1 and 300 seconds")
	}
	largest, err := hotstore.ImageSize(storage.MaxMessageBytes)
	if err != nil || largest > cfg.MaxPackBytes {
		return fmt.Errorf("max_pack_bytes must hold the largest accepted message including framing")
	}
	if cfg.ScratchDirectory == "" {
		return fmt.Errorf("storage lifecycle scratch_directory is required")
	}
	return nil
}

func (cfg *lifecycleConfig) openPacker(ctx context.Context, store *receiptstore.Store) (*receiptstore.Packer, error) {
	objects, err := cfg.Objects.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("object storage startup validation: %w", err)
	}
	directory, err := replicaconfig.Directory(cfg.ScratchDirectory)
	if err != nil {
		return nil, err
	}
	packer, err := receiptstore.NewPacker(
		ctx,
		store,
		objects,
		receiptstore.PackOptions{
			ScratchDirectory: directory,
			MaxPackBytes:     cfg.MaxPackBytes,
			MaxStagingBytes:  cfg.MaxStagingBytes,
			Lease:            time.Duration(cfg.PackLeaseSeconds) * time.Second,
		},
	)
	if err != nil {
		return nil, err
	}
	return packer, nil
}

func prepareMaintenance(
	store *receiptstore.Store,
	packer *receiptstore.Packer,
	cfg *lifecycleConfig,
	storage replicaconfig.Storage,
) (*receiptstore.Maintenance, []receiptstore.ID, error) {
	maintenance, err := receiptstore.NewMaintenance(
		store,
		packer,
		receiptstore.MaintenanceOptions{
			MaxActions:  cfg.MaxActions,
			MaxReceipts: cfg.MaxReceipts,
			MaxDuration: time.Duration(cfg.WorkTimeoutSeconds) * time.Second,
		},
	)
	if err != nil {
		return nil, nil, err
	}
	var members []receiptstore.ID
	for _, member := range storage.AllMembers() {
		id, e := replicaconfig.ParseID(member.ID)
		if e != nil {
			return nil, nil, e
		}
		members = append(members, id)
	}
	if cfg.ReclaimHot && len(members) == 0 {
		err := fmt.Errorf("hot reclamation requires members")
		return nil, nil, err
	}
	return maintenance, members, nil
}

func runLifecycle(
	ctx context.Context,
	store *receiptstore.Store,
	maintenance *receiptstore.Maintenance,
	cfg *lifecycleConfig,
	members []receiptstore.ID,
) {
	ticker := time.NewTicker(time.Duration(cfg.IntervalSeconds) * time.Second)
	defer ticker.Stop()
	reclaimCursor := hotReclaimCursor{rotate: cfg.RotateHot}
	nextReclaim := time.Now()
	for {
		cycle, stop := context.WithTimeout(ctx, time.Duration(cfg.WorkTimeoutSeconds)*time.Second)
		result, e := maintenance.RunOnce(cycle)
		logLifecycleProgress(ctx, result, e)
		nextReclaim = runLifecycleReclaim(ctx, cycle, store, cfg, members, &reclaimCursor, nextReclaim)
		stop()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func logLifecycleProgress(ctx context.Context, result receiptstore.MaintenanceResult, e error) {
	if e != nil && ctx.Err() == nil {
		log.Print("Storage lifecycle work failed; retained obligations will be retried")
	}
	if result.Packed+result.Compacted+result.Retired+result.Expired+result.Finished+result.Oversized > 0 {
		log.Printf(
			"Storage lifecycle: packed=%d compacted=%d retired=%d expired=%d removed=%d "+
				"oversized_sample=%d",
			result.Packed,
			result.Compacted,
			result.Retired,
			result.Expired,
			result.Finished,
			result.Oversized,
		)
	}
}

func runLifecycleReclaim(
	ctx, cycle context.Context,
	store *receiptstore.Store,
	cfg *lifecycleConfig,
	members []receiptstore.ID,
	reclaimCursor *hotReclaimCursor,
	nextReclaim time.Time,
) time.Time {
	var e error
	if cfg.ReclaimHot && cycle.Err() == nil && !time.Now().Before(nextReclaim) {
		e = reclaimCursor.run(cycle, store, members)
		nextReclaim = time.Now().Add(time.Duration(cfg.ReclaimIntervalSeconds) * time.Second)
		if e != nil && ctx.Err() == nil && !errors.Is(e, receiptstore.ErrBusy) && !errors.Is(e, receiptstore.ErrStale) {
			log.Print("Hot reclamation failed; byte obligations remain pending")
		}
	}
	return nextReclaim
}
