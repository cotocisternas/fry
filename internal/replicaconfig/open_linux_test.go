//go:build linux

package replicaconfig_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/dbschema"
	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/installation"
	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/replicaconfig"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func storagePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("FRY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL via scripts/tests/receipts.sh")
	}
	ctx := t.Context()
	control, err := pgxpool.New(ctx, dsn)
	requireReplicaNoError(t, err)
	t.Cleanup(control.Close)
	id, err := hotstore.NewID()
	requireReplicaNoError(t, err)
	name := "replicaconfig_" + id.String()
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := control.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := control.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	requireReplicaNoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = name
	requireReplicaNoError(t, dbschema.ConfigurePool(cfg))
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	requireReplicaNoError(t, err)
	t.Cleanup(pool.Close)
	requireReplicaNoError(t, installation.Initialize(ctx, pool))
	return pool
}

func localStorage(t *testing.T) replicaconfig.Storage {
	t.Helper()
	return replicaconfig.Storage{
		DataDirectory:          t.TempDir(),
		RecoveryRetentionHours: 24,
		MaxMessageBytes:        1 << 20,
		MemberCapacityBytes:    16 << 20,
		MaxInFlight:            2,
		Members: []replicaconfig.MemberConfig{
			{ID: "00000000000000000000000000000001", Domain: "zone-a"},
			{ID: "00000000000000000000000000000002", Domain: "zone-b"},
		},
	}
}

func openStorage(t *testing.T, c replicaconfig.Storage, pool *pgxpool.Pool) (*receiptstore.Store, func() error) {
	t.Helper()
	store, closeStore, err := c.Open(t.Context(), pool)
	requireReplicaNoError(t, err)
	t.Cleanup(func() {
		if err := closeStore(); err != nil {
			t.Error(err)
		}
	})
	return store, closeStore
}

// Observe durable files through their actual names, identities and byte hashes;
// no package-private member state is needed to prove refused startup is harmless.
func storageTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := fmt.Sprint(info.Mode())
		if info.Mode().IsRegular() {
			// os.Root confines reads to this test-owned temporary directory.
			dir, err := os.OpenRoot(root)
			if err != nil {
				return err
			}
			raw, readErr := dir.ReadFile(relative)
			closeErr := dir.Close()
			if err := errors.Join(readErr, closeErr); err != nil {
				return err
			}
			value = fmt.Sprintf("%s:%x", value, sha256.Sum256(raw))
		}
		tree[relative] = value
		return nil
	})
	requireReplicaNoError(t, err)
	return tree
}

func TestStorageOpenSecondMemberFailureReleasesFirstAndPreservesAcceptedOriginal(t *testing.T) {
	pool := storagePool(t)
	cfg := localStorage(t)
	store, closeStore := openStorage(t, cfg, pool)
	ctx := t.Context()
	org := receiptstore.ID{15: 3}
	attempt := receiptstore.ID{15: 4}
	envelope := receiptstore.Envelope{
		Organization: org,
		MailFrom:     "sender@example.test",
		Destinations: []receiptstore.Destination{
			{Inbox: receiptstore.ID{15: 5}, Recipients: []string{"inbox@example.test"}},
		},
	}
	const original = "Subject: preserve accepted original\r\n\r\nexact original bytes\r\n"
	accepted, err := store.Accept(ctx, attempt, envelope, io.NopCloser(bytes.NewBufferString(original)))
	requireReplicaNoError(t, err)
	requireReplicaNoError(t, closeStore())
	firstDir := filepath.Join(cfg.DataDirectory, cfg.Members[0].ID)
	secondDir := filepath.Join(cfg.DataDirectory, cfg.Members[1].ID)
	firstID, err := hotstore.InspectFileIdentity(firstDir)
	requireReplicaNoError(t, err)
	secondID, err := hotstore.InspectFileIdentity(secondDir)
	requireReplicaNoError(t, err)
	before := storageTree(t, cfg.DataDirectory)
	opts := hotstore.Options{
		MaxMessageBytes:      cfg.MaxMessageBytes,
		MaxBytes:             cfg.MemberCapacityBytes,
		MaxConcurrentAppends: cfg.MaxInFlight,
	}
	// Another real owner forces the second local open to fail after the first
	// succeeds. A partial result must never escape to the caller.
	second, err := receiptstore.OpenLocalMember(
		ctx,
		pool,
		receiptstore.ID{15: 2},
		cfg.Members[1].Domain,
		secondDir,
		opts,
	)
	requireReplicaNoError(t, err)
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	})
	assertPartialOpenClosed(t, cfg, pool, before)
	// Reacquiring first's public member lock proves failure cleanup released it.
	first, err := receiptstore.OpenLocalMember(ctx, pool, receiptstore.ID{15: 1}, cfg.Members[0].Domain, firstDir, opts)
	if err != nil {
		t.Fatalf("first member remained owned after second failed: %v", err)
	}
	requireReplicaNoError(t, first.Close())
	requireReplicaNoError(t, second.Close())
	reopened, closeReopened := openStorage(t, cfg, pool)
	verifyAcceptedAfterOpen(t, reopened, org, attempt, accepted, envelope, original)
	for directory, want := range map[string]receiptstore.ID{firstDir: firstID, secondDir: secondID} {
		requireStorageFileIdentity(t, directory, want)
	}
	requireReplicaNoError(t, closeReopened())
	if err := closeReopened(); err != nil {
		t.Fatalf("closer not idempotent: %v", err)
	}
	// A successful close also releases both local members for the next process.
	openStorage(t, cfg, pool)
}

func TestStorageOpenRefusesMissingCatalogBeforeCreatingByteFiles(t *testing.T) {
	pool := storagePool(t)
	cfg := localStorage(t)
	if _, err := pool.Exec(t.Context(), `ALTER TABLE fry_member_file RENAME TO incompatible_member_file`); err != nil {
		t.Fatal(err)
	}
	store, closeStore, err := cfg.Open(t.Context(), pool)
	if closeStore != nil {
		t.Cleanup(func() {
			if err := closeStore(); err != nil {
				t.Error(err)
			}
		})
	}
	if err == nil || store != nil || closeStore != nil {
		t.Fatalf("incompatible catalog opened: store=%v closer=%v err=%v", store, closeStore != nil, err)
	}
	tree := storageTree(t, cfg.DataDirectory)
	for path := range tree {
		if path != "." && path != cfg.Members[0].ID {
			t.Fatalf("schema refusal created a storage file or later member: %s", path)
		}
	}
}

func TestStorageOpenRejectsInvalidDirectoryWithoutChangingItsContents(t *testing.T) {
	cfg := localStorage(t)
	root, err := os.OpenRoot(cfg.DataDirectory)
	requireReplicaNoError(t, err)
	defer func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	}()
	const sentinel = "operator file must survive refused startup"
	requireReplicaNoError(t, root.WriteFile("blocked", []byte(sentinel), 0o600))
	before := storageTree(t, cfg.DataDirectory)
	cfg.DataDirectory = filepath.Join(cfg.DataDirectory, "blocked", "members")
	store, closeStore, err := cfg.Open(t.Context(), nil)
	if err == nil || store != nil || closeStore != nil {
		t.Fatalf("invalid directory opened: store=%v closer=%v err=%v", store, closeStore != nil, err)
	}
	if after := storageTree(t, root.Name()); !reflect.DeepEqual(before, after) {
		t.Fatalf("invalid directory mutated operator files: before=%v after=%v", before, after)
	}
}

func verifyAcceptedAfterOpen(
	t *testing.T,
	reopened *receiptstore.Store,
	org, attempt receiptstore.ID,
	accepted receiptstore.Receipt,
	envelope receiptstore.Envelope,
	original string,
) {
	t.Helper()
	ctx := t.Context()
	status, err := reopened.Status(ctx, org, attempt)
	if err != nil || status.State != "accepted" || status.Receipt == nil || status.Receipt.ID != accepted.ID {
		t.Fatalf("accepted status lost: %+v err=%v", status, err)
	}
	var got bytes.Buffer
	requireReplicaNoError(t, reopened.Read(ctx, org, accepted.ID, 0, accepted.Size, &got))
	if got.String() != original {
		t.Fatalf("original changed: %q", got.String())
	}
	replay, err := reopened.Accept(ctx, attempt, envelope, io.NopCloser(bytes.NewBufferString(original)))
	if err != nil || replay.ID != accepted.ID {
		t.Fatalf("accepted retry: %+v err=%v", replay, err)
	}
}

func assertPartialOpenClosed(t *testing.T, cfg replicaconfig.Storage, pool *pgxpool.Pool, before map[string]string) {
	t.Helper()
	ctx := t.Context()
	partial, partialClose, err := cfg.Open(ctx, pool)
	if partialClose != nil {
		t.Cleanup(func() {
			if err := partialClose(); err != nil {
				t.Error(err)
			}
		})
	}
	if !errors.Is(err, hotstore.ErrBusy) || partial != nil || partialClose != nil {
		t.Fatalf("partial open: store=%v closer=%v err=%v", partial, partialClose != nil, err)
	}
	if after := storageTree(t, cfg.DataDirectory); !reflect.DeepEqual(before, after) {
		t.Fatalf("refused open changed files: before=%v after=%v", before, after)
	}
}

func requireStorageFileIdentity(t *testing.T, directory string, want receiptstore.ID) {
	t.Helper()
	got, err := hotstore.InspectFileIdentity(directory)
	if err != nil || got != want {
		t.Fatalf("current file identity changed: got=%s want=%s err=%v", got, want, err)
	}
}
