//go:build linux

// Package installation composes the complete FRY database schema. Component
// fixture initialization is deliberately separate from this production boundary.
package installation

import (
	"context"

	"github.com/cotocisternas/fry/internal/admin"
	"github.com/cotocisternas/fry/internal/dbschema"
	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/jackc/pgx/v5/pgxpool"
)

func components() []dbschema.Component {
	return []dbschema.Component{
		receiptstore.SchemaDefinition(),
		admin.SchemaDefinition(),
		maildirectory.SchemaDefinition(),
	}
}

// Initialize creates all component schemas and their version ledger atomically.
// Existing installations are checked separately; no legacy schema is adopted.
func Initialize(ctx context.Context, pool *pgxpool.Pool) error {
	return dbschema.Initialize(ctx, pool, components())
}

// Check verifies the complete installation before any mail storage is opened.
// It changes neither schema nor mail authority and never performs an upgrade.
func Check(ctx context.Context, pool *pgxpool.Pool) error {
	return dbschema.Check(ctx, pool, components())
}
