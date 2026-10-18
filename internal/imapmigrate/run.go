//go:build linux

package imapmigrate

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cotocisternas/fry/internal/imapsource"
	"github.com/cotocisternas/fry/internal/receiptstore"
)

type migrationRun struct {
	migrator *Migrator
	pass     *receiptstore.MigrationPass
	source   *imapsource.Reader
	epochs   map[string]receiptstore.MigrationFolder
	result   Result
	visited  int
}

func (m *Migrator) executePass(
	ctx context.Context,
	source *imapsource.Reader,
	pass *receiptstore.MigrationPass,
) (Result, error) {
	inventory, err := pass.Inventory(ctx)
	if err != nil {
		return Result{}, err
	}
	run := &migrationRun{migrator: m, pass: pass, source: source, epochs: map[string]receiptstore.MigrationFolder{}}
	for _, folder := range inventory {
		run.epochs[folder.Source] = folder
	}
	if err = run.copyWindows(ctx); err != nil {
		return run.result, err
	}
	next, err := pass.NextWindow(ctx, imapsource.MaxWindow)
	if err != nil || next != nil {
		return run.result, err
	}
	err = run.reconcile(ctx)
	return run.result, err
}

func (r *migrationRun) copyWindows(ctx context.Context) error {
	cfg := r.migrator.config
	for r.result.Windows < cfg.MaxWindows && r.visited < cfg.MaxMessages {
		width := min(imapsource.MaxWindow, cfg.MaxMessages-r.visited)
		if width < 1 || width > imapsource.MaxWindow {
			return fmt.Errorf("imapmigrate: invalid window width")
		}
		window, err := r.pass.NextWindow(ctx, uint32(width))
		if err != nil {
			return err
		}
		if window == nil {
			return nil
		}
		if err = r.copyWindow(ctx, *window); err != nil {
			return err
		}
		r.result.Windows++
	}
	return nil
}

func (r *migrationRun) selectWindow(window receiptstore.MigrationWindow) error {
	captured, ok := r.epochs[window.Source]
	if !ok {
		return fmt.Errorf("imapmigrate: window outside captured inventory")
	}
	selected, err := r.source.Select(window.Source)
	if err != nil {
		return err
	}
	if selected.UIDValidity != captured.UIDValidity || uint64(selected.UIDNext) < captured.UIDNext ||
		(r.pass.Final && uint64(selected.UIDNext) != captured.UIDNext) {
		return fmt.Errorf("imapmigrate: source epoch or arrival watermark changed")
	}
	return nil
}

func (r *migrationRun) copyWindow(ctx context.Context, window receiptstore.MigrationWindow) error {
	if err := r.selectWindow(window); err != nil {
		return err
	}
	uids, err := r.source.SearchWindow(window.From, window.Through)
	if err != nil {
		return err
	}
	present := make([]uint32, 0, len(uids))
	for _, uid := range uids {
		r.visited++
		found, err := r.copyMessage(ctx, window, uid)
		if err != nil {
			return err
		}
		if found {
			present = append(present, uid)
		}
	}
	return r.pass.AdvanceWindow(ctx, window, present)
}

func (r *migrationRun) copyMessage(ctx context.Context, window receiptstore.MigrationWindow, uid uint32) (bool, error) {
	metadata, found, err := r.source.Metadata(uid)
	if err != nil {
		return false, err
	}
	if !found {
		return false, r.vanished(ctx, window, uid)
	}
	observed, err := r.pass.Observe(
		ctx,
		window,
		receiptstore.MigrationMessage{
			UID:          uid,
			Flags:        metadata.Flags,
			InternalDate: metadata.InternalDate,
			Size:         metadata.Size,
		},
	)
	if err != nil {
		return false, err
	}
	if observed.AlreadyPresent {
		r.result.AlreadyPresent++
		return true, nil
	}
	found, err = r.source.Fetch(
		uid,
		metadata.Size,
		func(body io.ReadCloser) error { return r.accept(ctx, observed, body) },
	)
	if err != nil {
		return false, err
	}
	if !found {
		return false, r.vanished(ctx, window, uid)
	}
	r.result.Copied++
	return true, nil
}

func (r *migrationRun) vanished(ctx context.Context, window receiptstore.MigrationWindow, uid uint32) error {
	if err := r.pass.Vanished(ctx, window, uid); err != nil {
		return err
	}
	r.result.Vanished++
	return nil
}

func (r *migrationRun) accept(ctx context.Context, observed receiptstore.MigrationCopy, body io.ReadCloser) error {
	_, err := r.migrator.store.Accept(ctx, observed.Attempt, observed.Envelope, body)
	if errors.Is(err, receiptstore.ErrUncertain) {
		status, statusErr := r.migrator.store.Status(ctx, r.migrator.config.Organization, observed.Attempt)
		if statusErr == nil && status.State == "accepted" {
			return nil
		}
	}
	return err
}

func (r *migrationRun) reconcile(ctx context.Context) error {
	// Reconciliation batches consume the inventory run's same work budget.
	for r.result.Windows < r.migrator.config.MaxWindows {
		removed, err := r.pass.Reconcile(ctx, imapsource.MaxWindow)
		if err != nil {
			return err
		}
		r.result.Removed += removed
		if removed == 0 {
			return r.seal(ctx)
		}
		r.result.Windows++
	}
	return nil
}

func (r *migrationRun) seal(ctx context.Context) error {
	observed, err := snapshot(r.source)
	if err != nil {
		return err
	}
	r.result.Manifest, err = r.pass.Seal(ctx, observed)
	if err != nil {
		return err
	}
	r.result.Complete = true
	return nil
}
