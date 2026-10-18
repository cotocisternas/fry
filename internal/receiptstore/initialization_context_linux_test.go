//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/testutil/s3fixture"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMemberInitializationContextOwnership(t *testing.T) {
	t.Run("canceled initialization leaves no registration", testCanceledMemberInitialization)
	t.Run("successful member outlives initialization", testMemberOutlivesInitialization)
}

func TestPackerInitializationContextOwnership(t *testing.T) {
	t.Run("canceled initialization leaves no registration", testCanceledPackerInitialization)
	t.Run("successful packer outlives initialization", testPackerOutlivesInitialization)
}

// The tracer observes the real PostgreSQL stage at the filesystem boundary. No
// global clock or store hook is changed, and the scan still opens real bytes.
type initializationStageTrace struct {
	mu     sync.Mutex
	latest context.Context
}

func (trace *initializationStageTrace) TraceQueryStart(
	ctx context.Context,
	_ *pgx.Conn,
	_ pgx.TraceQueryStartData,
) context.Context {
	trace.mu.Lock()
	trace.latest = ctx
	trace.mu.Unlock()
	return ctx
}
func (*initializationStageTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (trace *initializationStageTrace) context() context.Context {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return trace.latest
}

func TestMemberRestartScanDoesNotConsumeMetadataStageBudget(t *testing.T) {
	trace := &initializationStageTrace{}
	f := setupWithPoolConfig(
		t,
		func(config *pgxpool.Config) { config.ConnConfig.Tracer = trace },
		hotOptions(),
		Options{Lease: 10 * time.Second, RecoveryRetention: 24 * time.Hour},
	)
	body := "Subject: restart scan\r\n\r\naccepted bytes survive a long file scan"
	receipt := accept(t, f.s, newID(t), f.envelope(t), body)
	original := f.members[0]
	requireReceiptNoError(t, original.Close())
	parent, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	scanned := false
	member, err := openLocalMember(
		parent,
		f.pool,
		original.id,
		original.domain,
		f.dirs[0],
		hotOptions(),
		memberInitialization{
			metadataTimeout: time.Second,
			openStore: func(directory string, options hotstore.Options) (*hotstore.Store, error) {
				// A full scan can outlast the preceding metadata deadline. Waiting on that
				// real stage context makes the old shared-deadline bug deterministic.
				stage := trace.context()
				if stage == nil {
					return nil, errors.New("metadata stage was not observed")
				}
				select {
				case <-stage.Done():
				case <-parent.Done():
					return nil, parent.Err()
				}
				scanned = true
				return hotstore.Open(directory, options)
			},
		},
	)
	if member != nil {
		t.Cleanup(func() {
			if closeErr := member.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	requireReceiptCondition(t, scanned, "restart did not reach real file scan", err)
	requireReceiptCondition(t, err == nil, "completed scan exhausted the following metadata stage", err)
	requireRestartedMemberRead(t, parent, f, member, receipt, body)
}

func TestMemberInitializationCancellationDuringScanReleasesOwnership(t *testing.T) {
	f := setup(t)
	body := "Subject: canceled restart\r\n\r\naccepted bytes remain intact"
	receipt := accept(t, f.s, newID(t), f.envelope(t), body)
	original := f.members[0]
	requireReceiptNoError(t, original.Close())
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	member, err := openLocalMember(
		parent,
		f.pool,
		original.id,
		original.domain,
		f.dirs[0],
		hotOptions(),
		memberInitialization{
			metadataTimeout: time.Second,
			openStore: func(directory string, options hotstore.Options) (*hotstore.Store, error) {
				store, err := hotstore.Open(directory, options)
				cancel()
				return store, err
			},
		},
	)
	if member != nil {
		t.Cleanup(func() {
			if closeErr := member.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	requireReceiptCondition(t, errors.Is(err, context.Canceled), "cancellation during restart scan was ignored", err)
	requireReceiptCondition(t, member == nil, "cancellation during restart scan was ignored", err)
	reopened, err := OpenLocalMember(t.Context(), f.pool, original.id, original.domain, f.dirs[0], hotOptions())
	requireReceiptCondition(t, err == nil, "canceled initialization retained root ownership", err)
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	store, err := New(
		t.Context(),
		f.pool,
		[2]*LocalMember{reopened, f.members[1]},
		Options{Lease: 10 * time.Second, RecoveryRetention: 24 * time.Hour},
	)
	requireReceiptNoError(t, err)
	// Force this read through the restarted member, without replica fallback.
	requireReceiptNoError(t, f.members[1].Close())
	var output bytes.Buffer
	{
		err = store.Read(t.Context(), f.org, receipt.ID, 0, receipt.Size, &output)
		requireReceiptCondition(t, err == nil, "canceled restart changed accepted bytes", err)
		requireReceiptCondition(t, output.String() == body, "canceled restart changed accepted bytes", err)
	}
}

func testCanceledMemberInitialization(t *testing.T) {
	t.Helper()
	f := setup(t)
	id, directory := newID(t), t.TempDir()
	before := memberFilesTree(t, directory)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	member, err := OpenLocalMember(ctx, f.pool, id, "zone-c", directory, hotOptions())
	if member != nil {
		t.Cleanup(func() {
			if closeErr := member.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	requireReceiptCondition(t, errors.Is(err, context.Canceled), "canceled initialization opened member", err)
	requireReceiptCondition(t, member == nil, "canceled initialization opened member", err)
	for _, query := range []string{
		`SELECT count(*) FROM fry_member WHERE id=$1`,
		`SELECT count(*) FROM fry_member_file WHERE member=$1`,
		`SELECT count(*) FROM fry_member_profile WHERE member=$1`,
	} {
		requireReceiptCondition(
			t,
			scalar(t, f.pool, query, id.String()) == 0,
			"canceled initialization registered member state",
		)
	}
	memberFilesUnchanged(t, directory, before)
}

func testMemberOutlivesInitialization(t *testing.T) {
	t.Helper()
	f := setup(t)
	original := f.members[0]
	requireReceiptNoError(t, original.Close())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	member, err := OpenLocalMember(ctx, f.pool, original.id, original.domain, f.dirs[0], hotOptions())
	requireReceiptNoError(t, err)
	t.Cleanup(func() {
		if err := member.Close(); err != nil {
			t.Error(err)
		}
	})
	cancel()
	store, err := New(
		t.Context(),
		f.pool,
		[2]*LocalMember{member, f.members[1]},
		Options{Lease: 10 * time.Second, RecoveryRetention: 24 * time.Hour},
	)
	requireReceiptNoError(t, err)
	body := "Subject: member lifetime\r\n\r\noriginal after initialization cancellation"
	receipt := accept(t, store, newID(t), f.envelope(t), body)
	var output bytes.Buffer
	{
		err = store.Read(t.Context(), f.org, receipt.ID, 0, receipt.Size, &output)
		requireReceiptCondition(t, err == nil, "initialization cancellation stopped durable member operations", err)
		requireReceiptCondition(
			t,
			output.String() == body,
			"initialization cancellation stopped durable member operations",
			err,
		)
	}
}

func testCanceledPackerInitialization(t *testing.T) {
	t.Helper()
	f := setup(t)
	server := s3fixture.New(s3fixture.Config{})
	t.Cleanup(server.Close)
	adapter, options := namespaceAdapter(t, server, "fry-test"), namespacePackOptions(t)
	before := memberFilesTree(t, options.ScratchDirectory)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	packer, err := NewPacker(ctx, f.s, adapter, options)
	if packer != nil {
		t.Cleanup(func() {
			if closeErr := packer.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	requireReceiptCondition(t, errors.Is(err, context.Canceled), "canceled initialization opened packer", err)
	requireReceiptCondition(t, packer == nil, "canceled initialization opened packer", err)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_object_namespace WHERE namespace IS NOT NULL`) == 0,
		"canceled initialization registered packer state",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_pack`) == 0,
		"canceled initialization registered packer state",
	)
	memberFilesUnchanged(t, options.ScratchDirectory, before)
}

func testPackerOutlivesInitialization(t *testing.T) {
	t.Helper()
	f := setup(t)
	server := s3fixture.New(s3fixture.Config{})
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	packer, err := NewPacker(ctx, f.s, namespaceAdapter(t, server, "fry-test"), namespacePackOptions(t))
	requireReceiptNoError(t, err)
	t.Cleanup(func() {
		if err := packer.Close(); err != nil {
			t.Error(err)
		}
	})
	cancel()
	body := "Subject: packer lifetime\r\n\r\noriginal after initialization cancellation"
	receipt := accept(t, f.s, newID(t), f.envelope(t), body)
	job, err := packer.Pack(t.Context(), f.org, []ID{receipt.ID})
	requireReceiptCondition(t, err == nil, "initialization cancellation stopped packing", err)
	{
		state, err := packer.Status(t.Context(), f.org, job)
		requireReceiptCondition(t, err == nil, "pack was not published", state, err)
		requireReceiptCondition(t, state == "published", "pack was not published", state, err)
	}
	// Close both hot adapters so the public read must use the published object.
	for _, member := range f.members {
		requireReceiptNoError(t, member.Close())
	}
	var output bytes.Buffer
	{
		err = f.s.Read(t.Context(), f.org, receipt.ID, 0, receipt.Size, &output)
		requireReceiptCondition(t, err == nil, "initialization cancellation stopped cold reads", err)
		requireReceiptCondition(t, output.String() == body, "initialization cancellation stopped cold reads", err)
	}
}

func requireRestartedMemberRead(
	t *testing.T,
	parent context.Context,
	f *fixture,
	member *LocalMember,
	receipt Receipt,
	body string,
) {
	t.Helper()
	store, err := New(
		parent,
		f.pool,
		[2]*LocalMember{member, f.members[1]},
		Options{Lease: 10 * time.Second, RecoveryRetention: 24 * time.Hour},
	)
	requireReceiptNoError(t, err)
	// Force this read through the restarted member, without replica fallback.
	requireReceiptNoError(t, f.members[1].Close())
	var output bytes.Buffer
	{
		err = store.Read(parent, f.org, receipt.ID, 0, receipt.Size, &output)
		requireReceiptCondition(t, err == nil, "restart lost accepted bytes", err)
		requireReceiptCondition(t, output.String() == body, "restart lost accepted bytes", err)
	}
}
