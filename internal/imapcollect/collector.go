//go:build linux

// Package imapcollect copies external IMAP mail into FRY without mirroring later
// source changes. Production scheduling and secret administration are separate.
package imapcollect

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/cotocisternas/fry/internal/imapsource"
	"github.com/cotocisternas/fry/internal/receiptstore"
)

type (
	Mapping struct{ SourceFolder, DestinationFolder string }
	Config  struct {
		Connection, Organization, Inbox receiptstore.ID
		Address, Username, Password     string
		// Plaintext is only permitted with an explicitly configured loopback IP.
		LoopbackPlaintext bool
		Mappings          []Mapping
		DeleteSource      bool
		MaxMessages       int
		MaxMessageBytes   int64
		Timeout           time.Duration
	}
)

type (
	Result    struct{ Copied, AlreadyPresent, Vanished, Cleaned int }
	Collector struct {
		store  *receiptstore.Store
		config Config
	}
)

func New(store *receiptstore.Store, cfg Config) (*Collector, error) {
	if store == nil || !validConnection(cfg) || !validWorkLimits(cfg) {
		return nil, fmt.Errorf("imapcollect: invalid configuration")
	}

	host, _, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("imapcollect: address requires host and port")
	}
	if cfg.LoopbackPlaintext && !net.ParseIP(host).IsLoopback() {
		return nil, fmt.Errorf("imapcollect: plaintext requires loopback IP")
	}
	cfg.Mappings = append([]Mapping(nil), cfg.Mappings...)
	if err := validateMappings(cfg.Mappings); err != nil {
		return nil, err
	}

	return &Collector{store: store, config: cfg}, nil
}

func (c *Collector) RunOnce(parent context.Context) (Result, error) {
	ctx, cancel := context.WithTimeout(parent, c.config.Timeout)
	defer cancel()
	if err := c.bind(ctx); err != nil {
		return Result{}, err
	}
	reader, cleanup, err := imapsource.DialCollection(
		ctx,
		imapsource.Config{
			Address:           c.config.Address,
			Username:          c.config.Username,
			Password:          c.config.Password,
			LoopbackPlaintext: c.config.LoopbackPlaintext,
			MaxMessageBytes:   c.config.MaxMessageBytes,
		},
		c.config.DeleteSource,
	)
	if err != nil {
		return Result{}, err
	}
	defer closeSource(reader)
	run := &collectionRun{collector: c, reader: reader, sourceCleanup: cleanup}
	err = run.collectMappings(ctx)
	return run.result, err
}

func (c *Collector) bind(ctx context.Context) error {
	cfg := c.config
	identity, err := json.Marshal(struct {
		Address, Username string
		Plaintext         bool
	}{cfg.Address, cfg.Username, cfg.LoopbackPlaintext})
	if err != nil {
		return err
	}
	if err := c.store.BindSourceConnection(ctx, cfg.Organization, cfg.Connection, sha256.Sum256(identity)); err != nil {
		return err
	}
	return nil
}

func (c *Collector) cleanup(
	ctx context.Context,
	cleanup *imapsource.Cleanup,
	source receiptstore.Source,
	attempt receiptstore.ID,
) error {
	status, err := c.store.Status(ctx, c.config.Organization, attempt)
	if err != nil {
		return err
	}
	if status.State != "accepted" || status.Receipt == nil {
		return fmt.Errorf("imapcollect: cleanup requires an accepted receipt")
	}
	// A historic accepted marker alone must not authorize deleting source mail
	// after FRY has already expired its own retained copy.
	handle, err := c.store.Open(ctx, c.config.Organization, status.Receipt.ID, 0, status.Receipt.Size)
	if err != nil {
		return fmt.Errorf("imapcollect: retained receipt unavailable for source cleanup")
	}
	_, readErr := io.Copy(io.Discard, handle)
	if err := errors.Join(readErr, handle.Close()); err != nil {
		return err
	}

	return cleanup.Delete(
		imapsource.SourceMessage{Folder: source.Folder, UIDValidity: source.UIDValidity, UID: source.UID},
	)
}

func (c *Collector) acceptLiteral(
	ctx context.Context,
	attempt receiptstore.ID,
	envelope receiptstore.Envelope,
	reader io.ReadCloser,
) error {
	_, err := c.store.Accept(ctx, attempt, envelope, reader)
	if errors.Is(err, receiptstore.ErrUncertain) {
		status, e := c.store.Status(ctx, envelope.Organization, attempt)
		if e == nil && status.State == "accepted" {
			return nil
		}
	}
	return err
}

func validConnection(c Config) bool {
	return c.Connection != (receiptstore.ID{}) && c.Organization != (receiptstore.ID{}) &&
		c.Inbox != (receiptstore.ID{}) &&
		c.Username != "" &&
		c.Password != "" &&
		len(c.Mappings) > 0
}

func validWorkLimits(c Config) bool {
	return c.MaxMessages >= 1 && c.MaxMessages <= 10000 && c.MaxMessageBytes >= 1 && c.MaxMessageBytes <= 256<<20 &&
		c.Timeout >= time.Second
}

func validateMappings(mappings []Mapping) error {
	seen := map[string]bool{}
	for _, mapping := range mappings {
		key := canonicalInbox(mapping.SourceFolder)
		if key == "" || mapping.DestinationFolder == "" || seen[key] {
			return fmt.Errorf("imapcollect: invalid folder mapping")
		}
		seen[key] = true
	}
	return nil
}

func canonicalInbox(name string) string {
	if strings.EqualFold(name, "INBOX") {
		return "INBOX"
	}
	return name
}

// Close failures cannot change an already accepted receipt.
func closeSource(reader *imapsource.CollectionReader) {
	if err := reader.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Warn("IMAP collection connection cleanup failed")
	}
}
