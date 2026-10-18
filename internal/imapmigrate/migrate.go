//go:build linux

// Package imapmigrate coordinates bounded read-only history passes into an
// isolated receiptstore migration. Activation is a separate administrative act.
package imapmigrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/cotocisternas/fry/internal/imapsource"
	"github.com/cotocisternas/fry/internal/receiptstore"
)

type Config struct {
	Organization, Job, Inbox receiptstore.ID
	Source                   imapsource.Config
	MaxMessages, MaxWindows  int
	Timeout                  time.Duration
}
type Result struct {
	Copied, AlreadyPresent, Vanished, Windows, Removed int
	Complete                                           bool
	Manifest                                           [32]byte
}
type Migrator struct {
	store  *receiptstore.Store
	config Config
}

func New(store *receiptstore.Store, cfg Config) (*Migrator, error) {
	if store == nil || cfg.Organization == (receiptstore.ID{}) || cfg.Job == (receiptstore.ID{}) ||
		cfg.Inbox == (receiptstore.ID{}) ||
		cfg.MaxMessages < 1 ||
		cfg.MaxMessages > 10000 ||
		cfg.MaxWindows < 1 ||
		cfg.MaxWindows > 10000 ||
		cfg.Timeout < time.Second {
		return nil, fmt.Errorf("imapmigrate: invalid configuration")
	}
	return &Migrator{store, cfg}, nil
}

func (m *Migrator) Begin(
	parent context.Context,
	freeze *receiptstore.MigrationFreeze,
) (*receiptstore.MigrationPass, error) {
	ctx, cancel := context.WithTimeout(parent, m.config.Timeout)
	defer cancel()
	cfg := m.config
	if err := m.store.CreateMigration(
		ctx,
		cfg.Organization,
		cfg.Job,
		cfg.Inbox,
		receiptstore.FingerprintMigrationSource(cfg.Source.Address, cfg.Source.Username, cfg.Source.LoopbackPlaintext),
	); err != nil {
		return nil, err
	}
	source, err := imapsource.Dial(ctx, cfg.Source)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := source.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.WarnContext(ctx, "migration source connection cleanup failed")
		}
	}()
	inventory, err := snapshot(source)
	if err != nil {
		return nil, err
	}
	return m.store.BeginMigrationPass(ctx, cfg.Organization, cfg.Job, inventory, freeze)
}

func (m *Migrator) Resume(ctx context.Context) (*receiptstore.MigrationPass, error) {
	if err := m.bind(ctx); err != nil {
		return nil, err
	}
	return m.store.ResumeMigration(ctx, m.config.Organization, m.config.Job)
}

// mapFolders preserves a hierarchy by translating its delimiter to '/'. A slash
// that is data in another namespace cannot be represented without ambiguity.
func mapFolders(folders []imapsource.Folder) ([]receiptstore.MigrationFolder, error) {
	out := make([]receiptstore.MigrationFolder, 0, len(folders))
	seen := map[string]bool{}
	for _, f := range folders {
		name, err := destinationFolder(f)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("imapmigrate: ambiguous folder mapping")
		}

		seen[name] = true
		out = append(out, receiptstore.MigrationFolder{Source: f.Name, Destination: name, Subscribed: f.Subscribed})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out, nil
}

func snapshot(source *imapsource.Reader) ([]receiptstore.MigrationFolder, error) {
	folders, err := source.Discover()
	if err != nil {
		return nil, err
	}
	inventory, err := mapFolders(folders)
	if err != nil {
		return nil, err
	}
	for i := range inventory {
		epoch, err := source.Select(inventory[i].Source)
		if err != nil {
			return nil, err
		}
		inventory[i].UIDValidity = epoch.UIDValidity
		inventory[i].UIDNext = uint64(epoch.UIDNext)
	}
	return inventory, nil
}

func (m *Migrator) bind(ctx context.Context) error {
	cfg := m.config
	return m.store.CreateMigration(
		ctx,
		cfg.Organization,
		cfg.Job,
		cfg.Inbox,
		receiptstore.FingerprintMigrationSource(cfg.Source.Address, cfg.Source.Username, cfg.Source.LoopbackPlaintext),
	)
}

func (m *Migrator) Run(parent context.Context, pass *receiptstore.MigrationPass) (Result, error) {
	var result Result
	if pass == nil || pass.Organization != m.config.Organization || pass.Job != m.config.Job ||
		pass.Inbox != m.config.Inbox {
		return result, fmt.Errorf("imapmigrate: mismatched pass")
	}
	ctx, cancel := context.WithTimeout(parent, m.config.Timeout)
	defer cancel()
	if !pass.Deadline.IsZero() {
		var stop context.CancelFunc
		ctx, stop = context.WithDeadline(ctx, pass.Deadline)
		defer stop()
	}
	if err := m.bind(ctx); err != nil {
		return result, err
	}
	source, err := imapsource.Dial(ctx, m.config.Source)
	if err != nil {
		return result, err
	}
	defer func() {
		if err := source.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.WarnContext(ctx, "migration source connection cleanup failed")
		}
	}()
	return m.executePass(ctx, source, pass)
}

func destinationFolder(f imapsource.Folder) (string, error) {
	name := f.Name
	if f.Delimiter != '/' && strings.ContainsRune(name, '/') {
		return "", fmt.Errorf("imapmigrate: source slash is not hierarchy delimiter")
	}
	if f.Delimiter != 0 && f.Delimiter != '/' {
		name = strings.ReplaceAll(name, string(f.Delimiter), "/")
	}
	if strings.EqualFold(name, "INBOX") {
		name = "INBOX"
	}
	if name == "" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return "", fmt.Errorf("imapmigrate: ambiguous folder mapping")
	}
	return name, nil
}
