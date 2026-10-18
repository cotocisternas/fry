//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/objectstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

// Compact replaces a published pack with only its currently needed records.
// It returns a new job ID (or zero when there are no surviving records). The old
// pack enters retiring atomically with the swap; Retire drains it separately.
// Source bytes stay pinned during each copy. No live hot copies are removed here.
func (p *Packer) Compact(ctx context.Context, org, source ID) (ID, error) {
	if p.closed.Load() {
		return ID{}, hotstore.ErrClosed
	}
	if p.poisoned.Load() {
		return ID{}, fmt.Errorf("receiptstore: staging reconciliation required")
	}
	select {
	case p.slot <- struct{}{}:
		defer func() { <-p.slot }()
	default:
		return ID{}, ErrBusy
	}
	if p.closed.Load() {
		return ID{}, hotstore.ErrClosed
	}
	job, err := p.registerRewrite(ctx, org, source)
	if err != nil {
		return job.id, err
	}
	if len(job.entries) == 0 {
		return ID{}, nil
	}
	return p.build(ctx, job)
}

func packMembers(ctx context.Context, tx pgx.Tx, pack ID) ([]ID, error) {
	rows, err := tx.Query(ctx, dbgen.PackMembers, pack.String())
	if err != nil {
		return nil, err
	}
	var ids []ID
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			break
		}
		id, e := idFrom(raw)
		if e != nil {
			err = e
			break
		}
		ids = append(ids, id)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	if len(ids) == 0 || len(ids) > 1024 {
		return nil, ErrConflict
	}
	return ids, nil
}

func lockPacks(ctx context.Context, tx pgx.Tx, org ID, ids ...ID) error {
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	for _, id := range ids {
		err := func() error {
			sqlcResult, sqlcErr := dbgen.New(tx).
				LockPacks(ctx, dbgen.LockPacksParams{ID: id.String(), Organization: org.String()})
			if sqlcErr != nil {
				return sqlcErr
			}
			_ = sqlcResult
			return nil
		}()
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Packer) registerRewrite(ctx context.Context, org, source ID) (packJob, error) {
	job := packJob{org: org, source: source, epoch: 1}
	tx, err := p.receipts.db.begin(ctx)
	if err != nil {
		return job, err
	}
	defer rollback(ctx, tx)
	ids, err := publishedPackMembers(ctx, tx, org, source)
	if err != nil {
		return job, err
	}
	sizes, err := p.collectRewriteEntries(ctx, tx, &job, ids)
	if err != nil {
		return job, err
	}
	if err = guardRewriteSource(ctx, tx, job, len(ids)); err != nil {
		return job, err
	}
	if len(job.entries) == 0 {
		if _, err = dbgen.New(tx).PackerRegisterRewriteUpdateReceipt(ctx, new(source.String())); err != nil {
			return job, err
		}
		if _, err = dbgen.New(tx).PackerRegisterRewriteUpdatePack(ctx, source.String()); err != nil {
			return job, err
		}
		return job, commit(ctx, tx)
	}
	if err = p.registerRewriteReplacement(ctx, tx, &job, sizes); err != nil {
		return job, err
	}
	return job, commit(ctx, tx)
}

func (p *Packer) publishRewrite(ctx context.Context, job packJob, info objectstore.Info) error {
	tx, err := p.receipts.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	ids, err := packMembers(ctx, tx, job.source)
	if err != nil {
		return err
	}
	included, err := lockRewriteReceipts(ctx, tx, job, ids)
	if err != nil {
		return err
	}
	if err = lockPacks(ctx, tx, job.org, job.source, job.id); err != nil {
		return err
	}
	if err = packGuard(ctx, tx, job); err != nil {
		return err
	}
	if err = verifyRewriteImage(ctx, tx, job, info); err != nil {
		return err
	}
	if err = publishRewriteLocations(ctx, tx, job, info, ids, included); err != nil {
		return err
	}
	return commit(ctx, tx)
}

func publishedPackMembers(ctx context.Context, tx pgx.Tx, org, source ID) ([]ID, error) {
	// Scope before looking up membership; all authoritative locks still follow
	// receipt-then-container order. Published membership itself is immutable.
	var state string
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			PackerRegisterRewriteReadPack(ctx, dbgen.PackerRegisterRewriteReadPackParams{
				ID:           source.String(),
				Organization: org.String(),
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		state = sqlcResult
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if state != "published" {
		return nil, ErrStale
	}
	ids, err := packMembers(ctx, tx, source)
	if err != nil {
		return nil, err
	}
	return ids, nil
}

func (p *Packer) collectRewriteEntries(ctx context.Context, tx pgx.Tx, job *packJob, ids []ID) ([]int64, error) {
	var sizes []int64
	for _, id := range ids {
		state, live, version, e := p.rewriteEntryVersion(ctx, tx, job, id)
		if e != nil {
			return nil, e
		}
		if state == "live" && live {
			receipt, e := loadReceipt(ctx, tx, job.org, id, false)
			if e != nil {
				return nil, e
			}
			job.entries = append(job.entries, packEntry{receipt: receipt, version: version})
			sizes = append(sizes, receipt.Size)
		}
	}
	return sizes, nil
}

func guardRewriteSource(ctx context.Context, tx pgx.Tx, job packJob, originalCount int) error {
	var state string
	if err := lockPacks(ctx, tx, job.org, job.source); err != nil {
		return err
	}
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).PackerRegisterRewriteReadPackState(ctx, job.source.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		state = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	if state != "published" {
		return ErrStale
	}
	if len(job.entries) == originalCount {
		return ErrPackInUse
	} // No dead data to reclaim.
	return nil
}

func (p *Packer) registerRewriteReplacement(ctx context.Context, tx pgx.Tx, job *packJob, sizes []int64) error {
	size, err := hotstore.ImageSize(sizes...)
	if err != nil {
		return err
	}
	if size > p.opts.MaxPackBytes {
		return hotstore.ErrTooLarge
	}
	job.id, err = hotstore.NewID()
	if err != nil {
		return err
	}
	job.key = "packs/v1/" + job.id.String()[:2] + "/" + job.org.String() + "/" + job.id.String()
	if _, err = dbgen.New(tx).
		PackerRegisterRewriteInsertPack(ctx, dbgen.PackerRegisterRewriteInsertPackParams{
			ID:           job.id.String(),
			Organization: job.org.String(),
			ObjectKey:    job.key,
			Column4:      p.opts.Lease.Microseconds(),
			SourcePack:   new(job.source.String()),
			StagingOwner: new(p.staging.FileID().String()),
		}); err != nil {
		return err
	}
	for _, entry := range job.entries {
		if _, err = dbgen.New(tx).
			PackerRegisterRewriteInsertPackMember(ctx, dbgen.PackerRegisterRewriteInsertPackMemberParams{
				Pack:            job.id.String(),
				Receipt:         entry.receipt.ID.String(),
				ManifestVersion: entry.version,
			}); err != nil {
			return err
		}
	}
	return nil
}

func lockRewriteReceipts(ctx context.Context, tx pgx.Tx, job packJob, ids []ID) (map[ID]packEntry, error) {
	included := map[ID]packEntry{}
	for _, entry := range job.entries {
		included[entry.receipt.ID] = entry
	}
	for _, id := range ids {
		if err := guardRewriteReceipt(ctx, tx, job, id, included); err != nil {
			return nil, err
		}
	}
	return included, nil
}

func verifyRewriteImage(ctx context.Context, tx pgx.Tx, job packJob, info objectstore.Info) error {
	var state string
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).PackerPublishRewriteReadPack(ctx, job.source.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		state = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	if state != "published" {
		return ErrStale
	}
	var size int64
	var digest []byte
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).PackerPublishRewriteReadPackImageSizeImageSha256(ctx, job.id.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		if sqlcResult.ImageSize == nil {
			return fmt.Errorf(
				"receiptstore: unexpected NULL in PackerPublishRewriteReadPackImageSizeImageSha256.ImageSize",
			)
		}
		size = *sqlcResult.ImageSize
		digest = sqlcResult.ImageSha256
		return nil
	}(); err != nil {
		return err
	}
	if info.Retired || info.Size != size || string(info.SHA256[:]) != string(digest) {
		return objectstore.ErrIntegrity
	}
	return nil
}

func publishRewriteLocations(
	ctx context.Context,
	tx pgx.Tx,
	job packJob,
	info objectstore.Info,
	ids []ID,
	included map[ID]packEntry,
) error {
	for _, id := range ids {
		var next *string
		if _, ok := included[id]; ok {
			next = new(job.id.String())
		}
		if _, err := dbgen.New(tx).
			PackerPublishRewriteUpdateReceipt(ctx, dbgen.PackerPublishRewriteUpdateReceiptParams{
				ID:     id.String(),
				PackID: next,
			}); err != nil {
			return err
		}
	}
	if err := packGuard(ctx, tx, job); err != nil {
		return err
	}
	if err := livePackMembers(ctx, tx, job); err != nil {
		return err
	}
	if _, err := dbgen.New(tx).PackerPublishRewriteUpdatePack(ctx, job.source.String()); err != nil {
		return err
	}
	if _, err := dbgen.New(tx).
		PackerPublishRewriteUpdatePackPublished(ctx, dbgen.PackerPublishRewriteUpdatePackPublishedParams{
			ID:   job.id.String(),
			Etag: new(info.ETag),
		}); err != nil {
		return err
	}
	return nil
}

func (p *Packer) rewriteEntryVersion(ctx context.Context, tx pgx.Tx, job *packJob, id ID) (string, bool, int64, error) {
	state, err := lockReceipt(ctx, tx, job.org, id)
	if err != nil {
		return "", false, 0, err
	}
	live, err := p.receipts.reconcileLifetime(ctx, tx, id)
	if err != nil {
		return "", false, 0, err
	}
	row, err := dbgen.New(tx).PackerRegisterRewriteReadReceipt(ctx, id.String())
	if err != nil {
		return "", false, 0, err
	}
	if row.PackID == nil || *row.PackID != job.source.String() {
		return "", false, 0, ErrStale
	}
	return state, live, row.ManifestVersion, nil
}

func guardRewriteReceipt(ctx context.Context, tx pgx.Tx, job packJob, id ID, included map[ID]packEntry) error {
	state, err := lockReceipt(ctx, tx, job.org, id)
	if err != nil {
		return err
	}
	row, err := dbgen.New(tx).PackerPublishRewriteReadReference(ctx, id.String())
	if err != nil {
		return err
	}
	if row.PackID == nil || *row.PackID != job.source.String() {
		return ErrStale
	}
	entry, needed := included[id]
	if !validRewriteMembership(state, row.Exists, row.ManifestVersion, entry, needed) {
		return ErrStale
	}
	return nil
}

func validRewriteMembership(state string, live bool, version int64, entry packEntry, needed bool) bool {
	if needed {
		return state == "live" && live && version == entry.version
	}
	return state == "retiring" && !live
}
