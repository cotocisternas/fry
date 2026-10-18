//go:build linux

// Package imapcollect copies external IMAP mail into FRY without mirroring later
// source changes. Production scheduling and secret administration are separate.
package imapcollect

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
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
	conn, err := c.dial(ctx)
	if err != nil {
		return Result{}, err
	}
	defer closeConnection(conn)
	stop := context.AfterFunc(ctx, func() { closeConnection(conn) })
	defer stop()
	// Strict serialized FETCH routing validates identity even when UID follows a literal.
	client := imapclient.New(conn, &imapclient.Options{StrictFetchRouting: true})
	defer closeClient(client)
	if err = client.Login(c.config.Username, c.config.Password).Wait(); err != nil {
		return Result{}, fmt.Errorf("imapcollect: authentication failed")
	}
	if c.config.DeleteSource && !client.Caps().Has(imap.CapUIDPlus) {
		return Result{}, fmt.Errorf("imapcollect: safe source cleanup requires UIDPLUS")
	}
	run := &collectionRun{collector: c, client: client, conn: conn}
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

func (c *Collector) dial(ctx context.Context) (net.Conn, error) {
	cfg := c.config
	var err error
	dialer := net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	if cfg.LoopbackPlaintext {
		conn, err = dialer.DialContext(ctx, "tcp", cfg.Address)
	} else {
		host, _, splitErr := net.SplitHostPort(cfg.Address)
		if splitErr != nil {
			return nil, splitErr
		}
		tlsDialer := tls.Dialer{NetDialer: &dialer, Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}}
		conn, err = tlsDialer.DialContext(ctx, "tcp", cfg.Address)
	}
	if err != nil {
		return nil, fmt.Errorf("imapcollect: connection failed")
	}
	return conn, nil
}

func (c *Collector) cleanup(
	ctx context.Context,
	client *imapclient.Client,
	uid imap.UID,
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

	if _, err := client.Store(
		imap.UIDSetNum(uid),
		&imap.StoreFlags{
			Op:     imap.StoreFlagsAdd,
			Silent: true,
			Flags:  []imap.Flag{imap.FlagDeleted},
		},
		nil,
	).
		Collect(); err != nil {
		return fmt.Errorf("imapcollect: source cleanup flag failed")
	}
	if _, err := client.UIDExpunge(imap.UIDSetNum(uid)).Collect(); err != nil {
		return fmt.Errorf("imapcollect: source cleanup expunge failed")
	}
	return nil
}

// A truncated literal is never promoted to successful EOF. Closing an unfinished
// source closes the socket to unblock the protocol reader on cancellation.
type literalSource struct {
	reader    io.Reader
	remaining int64
	conn      net.Conn
	finished  atomic.Bool
	finalize  func() error
}

func (r *literalSource) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	if r.remaining < 0 {
		return n, fmt.Errorf("imapcollect: literal longer than announced")
	}
	if err == io.EOF {
		if r.remaining != 0 {
			return n, io.ErrUnexpectedEOF
		}
		if r.finalize != nil {
			if e := r.finalize(); e != nil {
				return n, e
			}
		}
		r.finished.Store(true)
	}
	return n, err
}

func (r *literalSource) Close() error {
	if !r.finished.Load() {
		return r.conn.Close()
	}
	return nil
}

func (c *Collector) copyLiteral(
	ctx context.Context,
	client *imapclient.Client,
	conn net.Conn,
	uid imap.UID,
	size int64,
	attempt receiptstore.ID,
	envelope receiptstore.Envelope,
) (bool, error) {
	section := &imap.FetchItemBodySection{Peek: true}
	cmd := client.Fetch(
		imap.UIDSetNum(uid),
		&imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}},
	)
	message := cmd.Next()
	if message == nil {
		if err := cmd.Close(); err != nil {
			return false, fmt.Errorf("imapcollect: source fetch failed")
		}
		return false, nil
	}
	fetch := &sourceFetch{cmd: cmd, message: message, section: section, conn: conn, uid: uid, size: size}
	reader, err := fetch.literal()
	if err != nil {
		abortFetch(conn, cmd)
		return false, err
	}
	if err = c.acceptLiteral(ctx, attempt, envelope, reader); err != nil {
		abortFetch(conn, cmd)
		return false, err
	}
	// Concurrent acceptance may close an unused stream; reconnect before cleanup.
	if !reader.finished.Load() {
		return false, fmt.Errorf("imapcollect: source already accepted concurrently; reconnect")
	}
	return true, nil
}

func (c *Collector) acceptLiteral(
	ctx context.Context,
	attempt receiptstore.ID,
	envelope receiptstore.Envelope,
	reader *literalSource,
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

type sourceFetch struct {
	cmd           *imapclient.FetchCommand
	message       *imapclient.FetchMessageData
	section       *imap.FetchItemBodySection
	conn          net.Conn
	uid, observed imap.UID
	size          int64
}

func (f *sourceFetch) literal() (*literalSource, error) {
	for item := f.message.Next(); item != nil; item = f.message.Next() {
		if value, ok := item.(imapclient.FetchItemDataUID); ok {
			f.observed = value.UID
			continue
		}
		body, ok := item.(imapclient.FetchItemDataBodySection)
		if !ok {
			continue
		}
		if body.Literal == nil || !body.MatchCommand(f.section) || body.Literal.Size() != f.size {
			return nil, fmt.Errorf("imapcollect: invalid source literal")
		}
		return &literalSource{reader: body.Literal, remaining: f.size, conn: f.conn, finalize: f.finish}, nil
	}
	return nil, fmt.Errorf("imapcollect: source omitted its literal")
}

func (f *sourceFetch) finish() error {
	// Successful EOF follows UID and the complete tagged response, never just
	// literal exhaustion. UID may legally arrive after the body literal.
	for tail := f.message.Next(); tail != nil; tail = f.message.Next() {
		if value, ok := tail.(imapclient.FetchItemDataUID); ok {
			f.observed = value.UID
		}
		if _, ok := tail.(imapclient.FetchItemDataBodySection); ok {
			return fmt.Errorf("imapcollect: duplicate source literal")
		}
	}
	if f.observed != f.uid {
		return fmt.Errorf("imapcollect: source UID mismatch")
	}
	if f.cmd.Next() != nil {
		return fmt.Errorf("imapcollect: extra source message")
	}
	if err := f.cmd.Close(); err != nil {
		return fmt.Errorf("imapcollect: source fetch did not complete")
	}
	return nil
}

func boundedUIDs(data *imap.SearchData) ([]imap.UID, error) {
	set, ok := data.All.(imap.UIDSet)
	if !ok {
		return nil, fmt.Errorf("imapcollect: source did not return UID search results")
	}
	var total uint64
	for _, r := range set {
		a, b := uint64(r.Start), uint64(r.Stop)
		if a == 0 || b == 0 {
			return nil, fmt.Errorf("imapcollect: dynamic source search result")
		}
		if a > b {
			a, b = b, a
		}
		total += b - a + 1
		if total > 1000000 {
			return nil, fmt.Errorf("imapcollect: source exceeds prototype search limit")
		}
	}
	return data.AllUIDs(), nil
}

// Connection cleanup must not turn an already accepted receipt into a retry.
func closeConnection(conn net.Conn) {
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Warn("IMAP collection connection cleanup failed")
	}
}

func closeClient(client *imapclient.Client) {
	if err := client.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Warn("IMAP collection client cleanup failed")
	}
}

func abortFetch(conn net.Conn, cmd *imapclient.FetchCommand) {
	closeConnection(conn)
	if err := cmd.Close(); err != nil {
		slog.Debug("IMAP collection fetch aborted with a protocol error")
	}
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
