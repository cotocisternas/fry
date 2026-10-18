//go:build linux

package receiptstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

// Vanished invalidates a background copy only after a successful source command
// proves that UID absent. A final pass treats this as a broken freeze assertion.
func (p *MigrationPass) Vanished(ctx context.Context, w MigrationWindow, uid uint32) error {
	if uid < w.From || uid > w.Through {
		return ErrConflict
	}
	tx, err := p.store.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err = p.guard(ctx, tx, false); err != nil {
		return err
	}
	if err = p.requireBackgroundMigrationWindow(ctx, tx, w); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).
		MigrationPassVanishedUpdateMigrationEntry(ctx, dbgen.MigrationPassVanishedUpdateMigrationEntryParams{
			Job:         p.Job.String(),
			SourceName:  w.Source,
			Uidvalidity: int64(w.UIDValidity),
			Uid:         int64(uid),
		}); err != nil {
		return err
	}
	if err = p.guard(ctx, tx, false); err != nil {
		return err
	}
	return commit(ctx, tx)
}

func (p *MigrationPass) requireScanned(ctx context.Context, tx pgx.Tx) error {
	var incomplete bool
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			MigrationPassRequireScanned(ctx, dbgen.MigrationPassRequireScannedParams{
				Job:            p.Job.String(),
				SeenGeneration: p.Generation,
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		if sqlcResult == nil {
			return fmt.Errorf("receiptstore: unexpected NULL in MigrationPassRequireScanned.column_1")
		}
		incomplete = *sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	if incomplete {
		return ErrBusy
	}
	return nil
}

// Reconcile removes at most limit memberships absent from a COMPLETED inventory.
// A partial or failed scan never authorizes removal. Receipts retain recovery
// references and ordinary physical cleanup obligations after membership removal.
func (p *MigrationPass) Reconcile(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1024 {
		return 0, ErrConflict
	}
	rows, err := dbgen.New(p.store.db.pool).
		MigrationPassReconcileReadMigrationEntry(ctx, dbgen.MigrationPassReconcileReadMigrationEntryParams{
			Job:            p.Job.String(),
			SeenGeneration: p.Generation,
			Limit:          int32(limit),
		})
	if err != nil {
		return 0, err
	}
	batch, receipts, folders := migrationReconcileBatch(rows)
	tx, err := p.store.db.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer rollback(ctx, tx)
	if err = p.lockMigrationReconciliation(ctx, tx, receipts, folders); err != nil {
		return 0, err
	}
	count, err := p.reconcileMigrationBatch(ctx, tx, batch)
	if err != nil {
		return 0, err
	}
	if err = p.guard(ctx, tx, false); err != nil {
		return 0, err
	}
	return count, commit(ctx, tx)
}

// Seal records a metadata/content-digest manifest after reconciliation and a
// fresh source namespace/UID-watermark consistency check. It does not claim that
// observations establish a source barrier or that payloads were re-downloaded.
func (p *MigrationPass) Seal(ctx context.Context, observed []MigrationFolder) ([32]byte, error) {
	var result [32]byte
	observed, _, err := normalizeMigrationInventory(observed)
	if err != nil {
		return result, err
	}
	tx, err := p.store.db.begin(ctx)
	if err != nil {
		return result, err
	}
	defer rollback(ctx, tx)
	if err = p.guard(ctx, tx, false); err != nil {
		return result, err
	}
	if err = p.requireScanned(ctx, tx); err != nil {
		return result, err
	}
	inventory, sourcePhase, err := p.checkMigrationSeal(ctx, tx, observed)
	if err != nil {
		return result, err
	}
	if err = p.sealMigrationNamespace(ctx, tx); err != nil {
		return result, err
	}
	result, count, err := p.migrationManifest(ctx, tx, inventory)
	if err != nil {
		return result, err
	}
	if err = p.recordMigrationSeal(ctx, tx, sourcePhase, result, count); err != nil {
		return result, err
	}
	// Recheck deadline after the potentially long manifest scan.
	if err = p.guard(ctx, tx, true); err != nil {
		return result, err
	}
	return result, commit(ctx, tx)
}

// Activate makes this inbox available and permanently fences migration writes.
// Routing/client switching is external orchestration, not implied by this call.
// An expired final deadline leaves staging isolated; it never weakens the gate.
func (p *MigrationPass) Activate(ctx context.Context) error {
	tx, err := p.store.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	current, err := dbgen.New(tx).
		MigrationPassActivateReadMigration(ctx, dbgen.MigrationPassActivateReadMigrationParams{
			ID:           p.Job.String(),
			Organization: p.Organization.String(),
		})
	if err != nil {
		return err
	}
	if current.Generation != p.Generation {
		return ErrStale
	}
	if current.Phase == "complete" {
		return nil
	}
	if current.Phase != "ready" {
		return ErrConflict
	}
	if err = p.guard(ctx, tx, true); err != nil {
		return err
	}
	if err = p.activateMigrationInbox(ctx, tx); err != nil {
		return err
	}
	return commit(ctx, tx)
}

// ReapMigrationAttempts aborts obsolete unaccepted imports in bounded batches.
// Generation replacement alone is never treated as proof of disk I/O closure.
func (s *Store) ReapMigrationAttempts(ctx context.Context, org, job ID, limit int) (int, error) {
	if limit < 1 || limit > 1024 {
		return 0, ErrConflict
	}
	rows, err := dbgen.New(s.db.pool).
		StoreReapMigrationAttempts(ctx, dbgen.StoreReapMigrationAttemptsParams{
			ID:           job.String(),
			Organization: org.String(),
			Limit:        int32(limit),
		})
	if err != nil {
		return 0, err
	}
	var ids []ID
	for _, sqlcRow := range rows {
		value := sqlcRow
		id, e := idFrom(value)
		if e != nil {
			err = e
			break
		}
		ids = append(ids, id)
	}
	if err != nil {
		return 0, err
	}
	count := 0
	for _, id := range ids {
		if err = s.Abort(ctx, org, id); err != nil && !errors.Is(err, ErrAccepted) {
			return count, err
		}
		count++
	}
	return count, nil
}

type staleMigrationMail struct{ entry, mail, receipt, folder string }

func migrationReconcileBatch(
	rows []dbgen.MigrationPassReconcileReadMigrationEntryRow,
) ([]staleMigrationMail, map[string]bool, map[string]bool) {
	var batch []staleMigrationMail
	receipts, folders := map[string]bool{}, map[string]bool{}
	for _, sqlcRow := range rows {
		var x staleMigrationMail
		x.entry = sqlcRow.ID
		x.mail = sqlcRow.ID_2
		x.receipt = sqlcRow.Receipt
		x.folder = sqlcRow.Folder
		batch = append(batch, x)
		receipts[x.receipt] = true
		folders[x.folder] = true
	}
	return batch, receipts, folders
}

func (p *MigrationPass) lockMigrationReconcileReceipts(ctx context.Context, tx pgx.Tx, receipts map[string]bool) error {
	keys := make([]string, 0, len(receipts))
	for r := range receipts {
		keys = append(keys, r)
	}
	sort.Strings(keys)
	for _, key := range keys {
		rid, e := idFrom(key)
		if e != nil {
			return e
		}
		if _, err := lockReceipt(ctx, tx, p.Organization, rid); err != nil {
			return err
		}
	}
	return nil
}

func lockMigrationReconcileFolders(ctx context.Context, tx pgx.Tx, folders map[string]bool) error {
	keys := make([]string, 0, len(folders))
	for f := range folders {
		keys = append(keys, f)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := func() error {
			sqlcResult, sqlcErr := dbgen.New(tx).MigrationPassReconcileReadFolder(ctx, key)
			if sqlcErr != nil {
				return sqlcErr
			}
			_ = sqlcResult
			return nil
		}(); err != nil {
			return err
		}
	}
	return nil
}

func (p *MigrationPass) reconcileMigrationBatch(
	ctx context.Context,
	tx pgx.Tx,
	batch []staleMigrationMail,
) (int, error) {
	count := 0
	for _, x := range batch {
		tag, e := dbgen.New(tx).
			MigrationPassReconcileUpdateMigrationEntry(ctx, dbgen.MigrationPassReconcileUpdateMigrationEntryParams{
				ID:             x.entry,
				Job:            p.Job.String(),
				SeenGeneration: p.Generation,
				Mail:           new(x.mail),
			})
		if e != nil {
			return 0, e
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		if _, err := dbgen.New(tx).MigrationPassReconcileDeleteMail(ctx, x.mail); err != nil {
			return 0, err
		}
		rid, e := idFrom(x.receipt)
		if e != nil {
			return 0, e
		}
		if err := releaseMailboxRef(ctx, tx, rid, "mailbox", x.mail); err != nil {
			return 0, err
		}
		if _, err := p.store.reconcileLifetime(ctx, tx, rid); err != nil {
			return 0, err
		}
		count++
	}
	return count, nil
}

func (p *MigrationPass) checkMigrationSeal(
	ctx context.Context,
	tx pgx.Tx,
	observed []MigrationFolder,
) ([]MigrationFolder, string, error) {
	var inventoryData []byte
	var sourcePhase string
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).MigrationPassSealReadMigration(ctx, p.Job.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		inventoryData = sqlcResult.Inventory
		sourcePhase = sqlcResult.Phase
		return nil
	}(); err != nil {
		return nil, "", err
	}
	var inventory []MigrationFolder
	if err := json.Unmarshal(inventoryData, &inventory); err != nil {
		return nil, "", err
	}
	if !reflect.DeepEqual(inventory, observed) {
		return nil, "", fmt.Errorf("receiptstore: source inventory changed during pass")
	}
	var inconsistent bool
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			MigrationPassSealCheckMigrationEntry(ctx, dbgen.MigrationPassSealCheckMigrationEntryParams{
				Job:            p.Job.String(),
				SeenGeneration: p.Generation,
				Inbox:          p.Inbox.String(),
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		if sqlcResult == nil {
			return fmt.Errorf("receiptstore: unexpected NULL in MigrationPassSealCheckMigrationEntry.column_1")
		}
		inconsistent = *sqlcResult
		return nil
	}(); err != nil {
		return nil, "", err
	}
	if inconsistent {
		return nil, "", ErrBusy
	}
	return inventory, sourcePhase, nil
}

func (p *MigrationPass) sealMigrationNamespace(ctx context.Context, tx pgx.Tx) error {
	// Obsolete folders are empty now and belong exclusively to this job.
	if _, err := dbgen.New(tx).
		MigrationPassSealDeleteFolder(ctx, dbgen.MigrationPassSealDeleteFolderParams{
			Inbox: p.Inbox.String(),
			Job:   p.Job.String(),
		}); err != nil {
		return err
	}
	if _, err := dbgen.New(tx).MigrationPassSealDeleteSubscription(ctx, p.Inbox.String()); err != nil {
		return err
	}
	if _, err := dbgen.New(tx).
		MigrationPassSealInsertSubscription(ctx, dbgen.MigrationPassSealInsertSubscriptionParams{
			Inbox: p.Inbox.String(),
			Job:   p.Job.String(),
		}); err != nil {
		return err
	}
	return nil
}

func (p *MigrationPass) migrationManifest(
	ctx context.Context,
	tx pgx.Tx,
	inventory []MigrationFolder,
) (result [32]byte, count int64, err error) {
	hash := sha256.New()
	enc := json.NewEncoder(hash)
	if err = enc.Encode(inventory); err != nil {
		return result, count, err
	}
	// Streaming row iteration avoids constructing a complete mailbox snapshot.
	rows, err := tx.Query(ctx, dbgen.MigrationPassSealReadMigrationEntry, p.Job.String(), p.Generation)
	if err != nil {
		return result, count, err
	}
	for rows.Next() {
		values, e := rows.Values()
		if e != nil {
			err = e
			break
		}
		if err = enc.Encode(values); err != nil {
			break
		}
		count++
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return result, count, err
	}
	if rowErr != nil {
		return result, count, rowErr
	}
	copy(result[:], hash.Sum(nil))
	return result, count, nil
}

func (p *MigrationPass) recordMigrationSeal(
	ctx context.Context,
	tx pgx.Tx,
	sourcePhase string,
	result [32]byte,
	count int64,
) error {
	phase := "copying"
	if sourcePhase == "final" {
		phase = "ready"
	}
	if _, err := dbgen.New(tx).
		MigrationPassSealUpdateMigration(ctx, dbgen.MigrationPassSealUpdateMigrationParams{
			ID:       p.Job.String(),
			Phase:    phase,
			Manifest: result[:],
		}); err != nil {
		return err
	}
	detail, err := json.Marshal(struct {
		Messages int64
		SHA256   string
	}{count, fmt.Sprintf("%x", result)})
	if err != nil {
		return err
	}
	if _, err := dbgen.New(tx).
		MigrationPassSealInsertMigrationEvent(ctx, dbgen.MigrationPassSealInsertMigrationEventParams{
			Job:        p.Job.String(),
			Generation: p.Generation,
			Detail:     detail,
		}); err != nil {
		return err
	}
	return nil
}

func (p *MigrationPass) activateMigrationInbox(ctx context.Context, tx pgx.Tx) error {
	tag, err := dbgen.New(tx).
		MigrationPassActivateUpdateInbox(ctx, dbgen.MigrationPassActivateUpdateInboxParams{
			ID:           p.Inbox.String(),
			MigrationJob: new(p.Job.String()),
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	if err = p.guard(ctx, tx, true); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).MigrationPassActivateUpdateMigration(ctx, p.Job.String()); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).
		MigrationPassActivateInsertMigrationEvent(ctx, dbgen.MigrationPassActivateInsertMigrationEventParams{
			Job:        p.Job.String(),
			Generation: p.Generation,
		}); err != nil {
		return err
	}
	var timely bool
	if err = func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			MigrationPassActivateReadMigrationDeadlineClockTimestamp(ctx, p.Job.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		timely = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	if !timely {
		return ErrStale
	}
	return nil
}

func (p *MigrationPass) requireBackgroundMigrationWindow(ctx context.Context, tx pgx.Tx, w MigrationWindow) error {
	phase, err := dbgen.New(tx).MigrationPassVanishedReadMigration(ctx, p.Job.String())
	if err != nil {
		return err
	}
	if phase != "copying" {
		return ErrConflict
	}
	valid, err := dbgen.New(tx).
		MigrationPassVanishedCheckMigrationFolder(ctx, dbgen.MigrationPassVanishedCheckMigrationFolderParams{
			Job:         p.Job.String(),
			SourceName:  w.Source,
			Uidvalidity: int64(w.UIDValidity),
			NextUid:     int64(w.From),
			UpperUid:    int64(w.Through),
		})
	if err != nil {
		return err
	}
	if !valid {
		return ErrStale
	}
	return nil
}

// Lock order matches cleanup: receipts, migration guard, then sorted folders.
// A complete scanned inventory is required before any membership removal.
func (p *MigrationPass) lockMigrationReconciliation(
	ctx context.Context,
	tx pgx.Tx,
	receipts, folders map[string]bool,
) error {
	if err := p.lockMigrationReconcileReceipts(ctx, tx, receipts); err != nil {
		return err
	}
	if err := p.guard(ctx, tx, false); err != nil {
		return err
	}
	if err := p.requireScanned(ctx, tx); err != nil {
		return err
	}
	if err := lockMigrationReconcileFolders(ctx, tx, folders); err != nil {
		return err
	}
	return nil
}
