//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrObjectNamespace = errors.New("receiptstore: immutable object namespace mismatch or unbound history")

// bindObjectNamespace is permanent: historical pack keys retain physical cleanup
// obligations even after retirement. A new namespace requires a new authority;
// old records cannot be adopted automatically, including empty/retired packs.
func (s *Store) bindObjectNamespace(ctx context.Context, namespace string) error {
	if !validObjectNamespace(namespace) {
		return fmt.Errorf("%w: invalid namespace", ErrObjectNamespace)
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	existing, err := dbgen.New(tx).StoreBindObjectNamespaceReadObjectNamespace(ctx)
	if err != nil {
		return err
	}
	if existing != nil {
		if *existing != namespace {
			return ErrObjectNamespace
		}
		return commit(ctx, tx)
	}
	// Exclude a concurrent legacy writer while checking first adoption. Normal
	// pack writers already pass this binding before obtaining a Packer.
	if _, err = tx.Exec(ctx, `LOCK TABLE fry_pack IN SHARE MODE`); err != nil {
		return err
	}
	history, err := dbgen.New(tx).StoreBindObjectNamespaceCheckPack(ctx)
	if err != nil {
		return err
	}
	if history {
		return ErrObjectNamespace
	}
	if _, err = dbgen.New(tx).StoreBindObjectNamespaceUpdateObjectNamespace(ctx, sqlcPointer(namespace)); err != nil {
		return err
	}
	return commit(ctx, tx)
}

// RequiresObjectStorage reports whether persisted storage history needs an object store.
func RequiresObjectStorage(ctx context.Context, db *pgxpool.Pool) (bool, error) {
	return dbgen.New(db).RequiresObjectStorage(ctx)
}

func validObjectNamespace(namespace string) bool {
	return namespace != "" && len(namespace) <= 4096
}
