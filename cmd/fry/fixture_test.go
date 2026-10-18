//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func commandDatabase(t *testing.T, prefix string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("FRY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx := t.Context()
	root, err := pgxpool.New(ctx, dsn)
	checkCommand(t, err)
	t.Cleanup(root.Close)
	schema := pgx.Identifier{prefix + rand.Text()}.Sanitize()
	_, err = root.Exec(ctx, "CREATE SCHEMA "+schema)
	checkCommand(t, err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := root.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	checkCommand(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	checkCommand(t, err)
	t.Cleanup(pool.Close)
	return pool
}
