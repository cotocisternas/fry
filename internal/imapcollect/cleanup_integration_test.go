//go:build linux

package imapcollect_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/imapcollect"
	"github.com/cotocisternas/fry/internal/imapserver"
	"github.com/cotocisternas/fry/internal/objectstore"
	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/testutil/s3fixture"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCleanupPreservesSourceUntilRetainedOriginalVerified(t *testing.T) {
	for _, failure := range []string{"payload read", "payload close"} {
		t.Run(failure, func(t *testing.T) {
			ctx := t.Context()
			scenario := prepareCleanupScenario(t)
			destination, address, raw := scenario.destination, scenario.address, scenario.raw
			cfg, identity, attempt, receipt := scenario.config, scenario.identity, scenario.attempt, scenario.receipt
			objects := packCleanupScenario(t, scenario, failure)
			assertCleanupPayloadFailure(t, scenario, objects, failure)

			cfg.DeleteSource = true
			collector, err := imapcollect.New(destination.store, cfg)
			requireTestCondition(t, err == nil, err)
			result, err := collector.RunOnce(ctx)
			requireTestConditionf(
				t,
				(errors.Is(err, objects.err) && result == (imapcollect.Result{AlreadyPresent: 1})),
				"failed retained verification must leave cleanup pending: result=%+v injectedFailure=%t",
				result,
				errors.Is(err, objects.err),
			)
			assertCleanupSource(t, address, imap.UID(identity.UID), raw)
			assertCleanupReceipt(t, destination, identity, attempt, receipt.ID)

			objects.enabled.Store(false)
			{
				result, err := collector.RunOnce(ctx)
				requireTestConditionf(
					t,
					(err == nil && result == (imapcollect.Result{AlreadyPresent: 1, Cleaned: 1})),
					"cleanup retry: result=%+v error=%v",
					result,
					err,
				)
			}
			assertCleanupSource(t, address, 0, "")
			assertCleanupReceipt(t, destination, identity, attempt, receipt.ID)
			handle, err := destination.store.Open(ctx, destination.org, receipt.ID, 0, receipt.Size)
			requireTestCondition(t, err == nil, err)
			body, readErr := io.ReadAll(handle)
			{
				err := errors.Join(readErr, handle.Close())
				requireTestCondition(t, err == nil, err)
			}
			requireTestCondition(t, string(body) == raw, "retained original changed after cleanup retry")
			{
				result, err := collector.RunOnce(ctx)
				requireTestConditionf(
					t,
					(err == nil && result == (imapcollect.Result{})),
					"completed cleanup repeated work: result=%+v error=%v",
					result,
					err,
				)
			}
		})
	}
}

// cleanupPayloadFailure injects failures only in the large payload range;
// framing and generation checks still use the real objectstore adapter.
type cleanupPayloadFailure struct {
	objectstore.Store
	payloadSize int64
	failure     string
	err         error
	enabled     atomic.Bool
}

func (s *cleanupPayloadFailure) Range(
	ctx context.Context,
	key, etag string,
	offset, length int64,
) (io.ReadCloser, error) {
	body, err := s.Store.Range(ctx, key, etag, offset, length)
	if err != nil || !s.enabled.Load() || length < s.payloadSize {
		return body, err
	}
	return &cleanupPayloadReader{ReadCloser: body, failure: s.failure, err: s.err, remaining: length / 2}, nil
}

type cleanupPayloadReader struct {
	io.ReadCloser
	failure   string
	err       error
	remaining int64
}

func (r *cleanupPayloadReader) Read(p []byte) (int, error) {
	if r.failure != "payload read" {
		return r.ReadCloser.Read(p)
	}
	if r.remaining == 0 {
		return 0, r.err
	}
	n, err := r.ReadCloser.Read(p[:min(int64(len(p)), r.remaining)])
	r.remaining -= int64(n)
	return n, err
}

func (r *cleanupPayloadReader) Close() error {
	err := r.ReadCloser.Close()
	if r.failure == "payload close" {
		return errors.Join(err, r.err)
	}
	return err
}

type cleanupStore struct {
	store      *receiptstore.Store
	members    [2]*receiptstore.LocalMember
	org, inbox receiptstore.ID
}

func cleanupID(t *testing.T) receiptstore.ID {
	t.Helper()
	id, err := hotstore.NewID()
	requireTestCondition(t, err == nil, err)
	return id
}

func newCleanupStore(t *testing.T) *cleanupStore {
	t.Helper()
	dsn := os.Getenv("FRY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("run scripts/tests/receipts.sh for database-backed collector tests")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	requireTestCondition(t, err == nil, err)
	t.Cleanup(admin.Close)
	schema := "fry_collect_" + cleanupID(t).String()
	identifier := pgx.Identifier{schema}.Sanitize()
	{
		_, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier)
		requireTestCondition(t, err == nil, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	config, err := pgxpool.ParseConfig(dsn)
	requireTestCondition(t, err == nil, err)
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	requireTestCondition(t, err == nil, err)
	t.Cleanup(pool.Close)
	{
		err := receiptstore.Migrate(ctx, pool)
		requireTestCondition(t, err == nil, err)
	}
	f := newCleanupMembers(t, ctx, pool)
	f.store, err = receiptstore.New(
		ctx,
		pool,
		f.members,
		receiptstore.Options{Lease: time.Second, RecoveryRetention: time.Hour},
	)
	requireTestCondition(t, err == nil, err)
	{
		err := f.store.EnsureInbox(ctx, f.org, f.inbox)
		requireTestCondition(t, err == nil, err)
	}
	return f
}

func (f *cleanupStore) closeMembers(t *testing.T) {
	t.Helper()
	for _, member := range f.members {
		{
			err := member.Close()
			requireTestCondition(t, err == nil, err)
		}
	}
}

func (f *cleanupStore) serve(t *testing.T) string {
	t.Helper()
	server, err := imapserver.New(
		f.store,
		[]imapserver.Account{
			{Username: "test", Password: "synthetic-only-password", Organization: f.org, Inbox: f.inbox},
		},
	)
	requireTestCondition(t, err == nil, err)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	requireTestCondition(t, err == nil, err)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
		if err := <-done; err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	return listener.Addr().String()
}

func cleanupClient(t *testing.T, address string) *imapclient.Client {
	t.Helper()
	client, err := imapclient.DialInsecure(address, nil)
	requireTestCondition(t, err == nil, err)
	t.Cleanup(func() {
		if err := client.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	{
		err := client.Login("test", "synthetic-only-password").Wait()
		requireTestCondition(t, err == nil, err)
	}
	return client
}

func assertCleanupSource(t *testing.T, address string, uid imap.UID, raw string) {
	t.Helper()
	client := cleanupClient(t, address)
	{
		_, err := client.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
		requireTestCondition(t, err == nil, err)
	}
	search, err := client.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	requireTestCondition(t, err == nil, err)
	if uid == 0 {
		requireTestCondition(t, len(search.AllUIDs()) == 0, "successfully verified source message was not expunged")
		return
	}
	requireTestCondition(
		t,
		slices.Equal(search.AllUIDs(), []imap.UID{uid}),
		"source message missing after retained verification failed",
	)
	section := &imap.FetchItemBodySection{Peek: true}
	messages, err := client.Fetch(
		imap.UIDSetNum(uid),
		&imap.FetchOptions{
			UID:         true,
			Flags:       true,
			BodySection: []*imap.FetchItemBodySection{section},
		},
	).
		Collect()
	requireTestConditionf(
		t,
		(err == nil && len(messages) == 1),
		"source verification fetch: count=%d error=%v",
		len(messages),
		err,
	)
	message := messages[0]
	requireTestCondition(
		t,
		(((message.UID == uid && string(message.FindBodySection(section)) == raw) &&
			slices.Contains(message.Flags, imap.FlagFlagged)) &&
			!slices.Contains(message.Flags, imap.FlagDeleted)),
		"failed cleanup changed source identity, original bytes, or deletion flags",
	)
}

func assertCleanupReceipt(t *testing.T, f *cleanupStore, source receiptstore.Source, attempt, receipt receiptstore.ID) {
	t.Helper()
	gotAttempt, _, status, err := f.store.SourceAttempt(t.Context(), f.org, source)
	requireTestConditionf(
		t,
		((((err == nil && gotAttempt == attempt) && status.State == "accepted") &&
			status.Receipt != nil) &&
			status.Receipt.ID == receipt),
		"cleanup changed durable source receipt: state=%s error=%v",
		status.State,
		err,
	)
	{
		_, err := f.store.PublishMailboxes(t.Context(), 10)
		requireTestCondition(t, err == nil, err)
	}
	_, messages, err := f.store.Mailbox(t.Context(), f.org, f.inbox, "INBOX")
	requireTestConditionf(
		t,
		((err == nil && len(messages) == 1) && messages[0].Receipt == receipt),
		"cleanup duplicated or replaced destination mail: count=%d error=%v",
		len(messages),
		err,
	)
}

type cleanupScenario struct {
	destination  *cleanupStore
	address, raw string
	config       imapcollect.Config
	identity     receiptstore.Source
	attempt      receiptstore.ID
	receipt      receiptstore.Receipt
}

func prepareCleanupScenario(t *testing.T) cleanupScenario {
	t.Helper()
	ctx := t.Context()
	source := newCleanupStore(t)
	destination := newCleanupStore(t)
	address := source.serve(t)
	raw := "From: synthetic@example.test\r\nSubject: cleanup verification\r\n\r\n" + strings.Repeat(
		"original\x00MIME\r\n",
		12000,
	)
	client := cleanupClient(t, address)
	appended := appendCleanupOriginal(t, client, raw)
	cfg := imapcollect.Config{
		Connection:        cleanupID(t),
		Organization:      destination.org,
		Inbox:             destination.inbox,
		Address:           address,
		Username:          "test",
		Password:          "synthetic-only-password",
		LoopbackPlaintext: true,
		Mappings:          []imapcollect.Mapping{{SourceFolder: "INBOX", DestinationFolder: "INBOX"}},
		MaxMessages:       10,
		MaxMessageBytes:   1 << 20,
		Timeout:           20 * time.Second,
	}
	collector, err := imapcollect.New(destination.store, cfg)
	requireTestCondition(t, err == nil, err)
	{
		result, err := collector.RunOnce(ctx)
		requireTestConditionf(
			t,
			(err == nil && result == (imapcollect.Result{Copied: 1})),
			"initial collection: result=%+v error=%v",
			result,
			err,
		)
	}
	identity := receiptstore.Source{
		Connection:  cfg.Connection,
		Folder:      "INBOX",
		UIDValidity: appended.UIDValidity,
		UID:         uint32(appended.UID),
	}
	attempt, _, accepted, err := destination.store.SourceAttempt(ctx, destination.org, identity)
	requireTestConditionf(
		t,
		((err == nil && accepted.State == "accepted") && accepted.Receipt != nil),
		"initial source receipt unavailable: state=%s error=%v",
		accepted.State,
		err,
	)
	receipt := *accepted.Receipt
	{
		_, err := destination.store.PublishMailboxes(ctx, 10)
		requireTestCondition(t, err == nil, err)
	}
	return cleanupScenario{destination, address, raw, cfg, identity, attempt, receipt}
}

func packCleanupScenario(t *testing.T, scenario cleanupScenario, failure string) *cleanupPayloadFailure {
	t.Helper()
	ctx := t.Context()
	destination, raw, receipt := scenario.destination, scenario.raw, scenario.receipt

	server := s3fixture.New(s3fixture.Config{})
	t.Cleanup(server.Close)
	adapter, err := objectstore.NewS3(ctx, server.Client(), server.Client(), "fry-test", "")
	requireTestCondition(t, err == nil, err)
	objects := &cleanupPayloadFailure{
		Store:       adapter,
		payloadSize: int64(len(raw)),
		failure:     failure,
		err:         errors.New("synthetic retained payload failure"),
	}
	packer, err := receiptstore.NewPacker(
		ctx,
		destination.store,
		objects,
		receiptstore.PackOptions{
			ScratchDirectory: t.TempDir(),
			MaxPackBytes:     4 << 20,
			MaxStagingBytes:  16 << 20,
			Lease:            10 * time.Second,
		},
	)
	requireTestCondition(t, err == nil, err)
	t.Cleanup(func() {
		if err := packer.Close(); err != nil {
			t.Error(err)
		}
	})
	{
		_, err := packer.Pack(ctx, destination.org, []receiptstore.ID{receipt.ID})
		requireTestCondition(t, err == nil, err)
	}
	// Make the published pack the only readable copy through the actual
	// adapters. The source has separate members and remains available.
	destination.closeMembers(t)
	return objects
}

func assertCleanupPayloadFailure(
	t *testing.T,
	scenario cleanupScenario,
	objects *cleanupPayloadFailure,
	failure string,
) {
	t.Helper()
	ctx := t.Context()
	destination, receipt := scenario.destination, scenario.receipt

	objects.enabled.Store(true)
	// Even a payload Close failure reaches the draining caller through
	// io.Copy. It is not a separate handle.Close-only failure case.
	probe, err := destination.store.Open(ctx, destination.org, receipt.ID, 0, receipt.Size)
	requireTestCondition(t, err == nil, err)
	copied, readErr := io.Copy(io.Discard, probe)
	closeErr := probe.Close()
	requireTestConditionf(
		t,
		(errors.Is(readErr, objects.err) && (closeErr == nil || errors.Is(closeErr, objects.err))),
		"payload failure did not reach io.Copy: injectedFailure=%t closeError=%v",
		errors.Is(readErr, objects.err),
		closeErr,
	)
	requireTestConditionf(
		t,
		((failure != "payload close" || copied == receipt.Size) &&
			(failure != "payload read" || (copied > 0 && copied < receipt.Size))),
		"payload failure occurred at wrong point: copied=%d size=%d",
		copied,
		receipt.Size,
	)
}

func newCleanupMembers(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *cleanupStore {
	t.Helper()
	var err error
	f := &cleanupStore{org: cleanupID(t), inbox: cleanupID(t)}
	for i := range f.members {
		f.members[i], err = receiptstore.OpenLocalMember(
			ctx,
			pool,
			cleanupID(t),
			fmt.Sprintf("collector-local-%d", i),
			t.TempDir(),
			hotstore.Options{MaxMessageBytes: 1 << 20, MaxBytes: 16 << 20, MaxConcurrentAppends: 2},
		)
		requireTestCondition(t, err == nil, err)
		member := f.members[i]
		t.Cleanup(func() {
			if err := member.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	return f
}

func appendCleanupOriginal(t *testing.T, client *imapclient.Client, raw string) *imap.AppendData {
	t.Helper()
	appendCommand := client.Append("INBOX", int64(len(raw)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagFlagged}})
	{
		_, err := io.Copy(appendCommand, strings.NewReader(raw))
		requireTestCondition(t, err == nil, err)
	}
	{
		err := appendCommand.Close()
		requireTestCondition(t, err == nil, err)
	}
	appended, err := appendCommand.Wait()
	requireTestCondition(t, err == nil, err)
	return appended
}
