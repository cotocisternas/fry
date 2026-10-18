//go:build linux

package receiptstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"
)

// Close drains pack construction before releasing scratch-directory ownership.
// Callers must stop new work first. Cold object reads do not use these files.
func (p *Packer) Close() error {
	p.slot <- struct{}{}
	defer func() { <-p.slot }()
	if p.closed.Swap(true) {
		return nil
	}
	p.receipts.cold.CompareAndSwap(p, nil)
	return p.staging.Close()
}

// The root's durable file incarnation identifies its staging jobs. Its flock
// excludes other builders; reacquisition after process exit makes leftover
// local scratch safe to remove. Revoke unpublished jobs before touching files.
func (p *Packer) reconcileStaging(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	ids, err := p.stagingPackIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = p.reconcileStagingPack(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (p *Packer) stagingPackIDs(ctx context.Context) ([]ID, error) {
	rows, err := p.receipts.db.pool.Query(ctx, dbgen.PackerReconcileStagingReadPack, p.staging.FileID().String())
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
	return ids, nil
}

func (p *Packer) reconcileStagingPack(ctx context.Context, id ID) error {
	if err := p.revokeStagingPack(ctx, id); err != nil {
		return err
	}
	dir := filepath.Join(p.opts.ScratchDirectory, "fry-pack-"+id.String())
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := syncMemberRoot(p.opts.ScratchDirectory); err != nil {
		return err
	}
	return p.finishStagingPack(ctx, id)
}

func (p *Packer) revokeStagingPack(ctx context.Context, id ID) error {
	tx, err := p.receipts.db.begin(ctx)
	if err != nil {
		return err
	}
	_, err = dbgen.New(tx).PackerReconcileStagingUpdatePack(ctx, id.String())
	if err == nil {
		err = commit(ctx, tx)
	} else {
		rollback(ctx, tx)
	}
	if err != nil {
		return err
	}
	return nil
}

func (p *Packer) finishStagingPack(ctx context.Context, id ID) error {
	tx, err := p.receipts.db.begin(ctx)
	if err != nil {
		return err
	}
	tag, err := dbgen.New(tx).
		PackerReconcileStagingUpdatePack2(ctx, dbgen.PackerReconcileStagingUpdatePack2Params{
			ID:           id.String(),
			StagingOwner: sqlcPointer(p.staging.FileID().String()),
		})
	if err == nil && tag.RowsAffected() != 1 {
		err = fmt.Errorf("receiptstore: staging ownership changed")
	}
	if err == nil {
		err = commit(ctx, tx)
	} else {
		rollback(ctx, tx)
	}
	if err != nil {
		return err
	}
	return nil
}
