//go:build linux

package receiptstore

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OrganizationStorageUsage counts each committed original once until its
// lifecycle becomes removed. It includes retiring cleanup debt, outbound and
// staged history originals. It excludes preparing/aborted bytes and does not
// measure physical hot copies, pack/staging overhead, headers or filesystem use.
// A live lifecycle row may already have expired references awaiting a sweep.
type OrganizationStorageUsage struct {
	Organization      ID
	RetainedOriginals int64
	RetainedBytes     int64
}

func checkStorageUsageSchema(ctx context.Context, pool *pgxpool.Pool) error {
	var version int
	var algorithm string
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(pool).CheckStorageUsageSchemaReadStorageAccounting(ctx)
		if sqlcErr != nil {
			return sqlcErr
		}
		version = int(sqlcResult.Version)
		algorithm = sqlcResult.Algorithm
		return nil
	}(); err != nil {
		return fmt.Errorf("receiptstore: initialized storage accounting schema required: %w", err)
	}
	if version != 1 || algorithm != "receipt-sha256-64-v1" {
		return fmt.Errorf("receiptstore: unsupported storage accounting schema")
	}
	_, err := dbgen.New(pool).CheckStorageUsageSchemaReadStorageUsage(ctx)
	if err != nil {
		return fmt.Errorf("receiptstore: initialized storage accounting schema required: %w", err)
	}
	return nil
}

// storageUsageSlot is immutable for the lifetime of this accounting version.
func storageUsageSlot(receipt ID) int16 {
	hash := sha256.Sum256(append([]byte("fry/storage-usage/v1/"), receipt[:]...))
	return int16(hash[0] & 63)
}

// Receipt accounting is the LAST locking write in its transaction. No caller
// may acquire further receipt/folder/profile/migration locks after this point.
// Acceptance's repeated deadline guards only revisit already-held authority.
func addRetainedStorage(ctx context.Context, tx pgx.Tx, receipt ID) error {
	tag, err := dbgen.New(tx).
		AddRetainedStorage(ctx, dbgen.AddRetainedStorageParams{ID: receipt.String(), Slot: storageUsageSlot(receipt)})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("receiptstore: missing receipt for storage accounting")
	}
	return nil
}

func removeRetainedStorage(ctx context.Context, tx pgx.Tx, org, receipt ID, size int64) error {
	tag, err := dbgen.New(tx).
		RemoveRetainedStorage(ctx, dbgen.RemoveRetainedStorageParams{
			Organization:  org.String(),
			Slot:          storageUsageSlot(receipt),
			RetainedBytes: size,
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("receiptstore: retained storage accounting underflow or missing stripe")
	}
	return nil
}

type storageUsageReader interface {
	dbgen.DBTX
}

func readOrganizationStorageUsage(ctx context.Context, q storageUsageReader, org ID) (OrganizationStorageUsage, error) {
	result := OrganizationStorageUsage{Organization: org}
	if org == (ID{}) {
		return result, ErrConflict
	}
	// SUM(bigint) uses PostgreSQL numeric; its checked cast fails visibly if the
	// installation outgrows the public int64 representation. Never wrap/saturate.
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(q).ReadOrganizationStorageUsage(ctx, org.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		result.RetainedOriginals = sqlcResult.Column1
		result.RetainedBytes = sqlcResult.Column2
		return nil
	}()
	return result, err
}

// OrganizationStorageUsage reads at most 64 stripe rows in one MVCC statement.
// It neither reads quota policy nor grants/deletes any storage authority.
func (s *Store) OrganizationStorageUsage(ctx context.Context, org ID) (OrganizationStorageUsage, error) {
	return readOrganizationStorageUsage(ctx, s.db.pool, org)
}

// ReadOrganizationStorageUsage reuses an authorized caller's transaction. It
// never checks out another connection while that transaction holds pool space.
func ReadOrganizationStorageUsage(ctx context.Context, tx pgx.Tx, org ID) (OrganizationStorageUsage, error) {
	return readOrganizationStorageUsage(ctx, tx, org)
}
