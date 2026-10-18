//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/objectstore"
	"github.com/cotocisternas/fry/internal/testutil/s3fixture"
)

type interceptedObjects struct {
	objectstore.Store
	create func(context.Context, string, io.ReadSeeker, int64, [32]byte) error
}

func (o *interceptedObjects) Create(ctx context.Context, key string, r io.ReadSeeker, n int64, d [32]byte) error {
	if o.create != nil {
		return o.create(ctx, key, r, n, d)
	}
	return o.Store.Create(ctx, key, r, n, d)
}

func packing(t *testing.T) (*fixture, *Packer, *s3fixture.Fixture, *interceptedObjects) {
	t.Helper()
	f := setup(t)
	server := s3fixture.New(s3fixture.Config{})
	t.Cleanup(server.Close)
	adapter, err := objectstore.NewS3(context.Background(), server.Client(), server.Client(), "fry-test", "")
	requireReceiptNoError(t, err)
	objects := &interceptedObjects{Store: adapter}
	p, err := NewPacker(
		context.Background(),
		f.s,
		objects,
		PackOptions{
			ScratchDirectory: t.TempDir(),
			MaxPackBytes:     4 << 20,
			MaxStagingBytes:  16 << 20,
			Lease:            10 * time.Second,
		},
	)
	requireReceiptNoError(t, err)
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	return f, p, server, objects
}

func objectKey(t *testing.T, f *fixture, id ID) string {
	t.Helper()
	var key string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(context.Background(), "SELECT object_key FROM fry_pack WHERE id=$1", id.String()).Scan(&key),
	)
	return key
}

func TestPackPublicationAndColdRanges(t *testing.T) {
	f, p, server, _ := packing(t)
	body := strings.Repeat("raw\x00MIME\r\n", 150000)
	r1 := accept(t, f.s, newID(t), f.envelope(t), body)
	r2 := accept(t, f.s, newID(t), f.envelope(t), "")
	id, err := p.Pack(context.Background(), f.org, []ID{r2.ID, r1.ID})
	requireReceiptNoError(t, err)
	{
		state, err := p.Status(context.Background(), f.org, id)
		requireReceiptCondition(t, err == nil, state, err)
		requireReceiptCondition(t, state == "published", state, err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_receipt WHERE pack_id IS NOT NULL") == 2,
		"incomplete publication",
	)
	for _, m := range f.members {
		requireReceiptCondition(t, len(m.bytes.Inventory()) == 2, "hot copies reclaimed prematurely")
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	}
	before := len(server.Requests())
	var out bytes.Buffer
	{
		err = f.s.Read(context.Background(), f.org, r1.ID, 0, r1.Size, &out)
		requireReceiptCondition(t, err == nil, "cold full read", err)
		requireReceiptCondition(t, out.String() == body, "cold full read", err)
	}
	{
		got := len(server.Requests()) - before
		requireReceiptConditionf(t, got == 4, "cold full read used %d requests, want 4 independent of frame count", got)
	}
	out.Reset()
	offset := int64(64*1024 - 3)
	{
		err = f.s.Read(context.Background(), f.org, r1.ID, offset, 31, &out)
		requireReceiptCondition(t, err == nil, "cold range read", err)
		requireReceiptCondition(t, out.String() == body[offset:offset+31], "cold range read", err)
	}
	{
		err = f.s.Read(context.Background(), f.org, r2.ID, 0, 0, io.Discard)
		requireReceiptCondition(t, err == nil, "empty cold record", err)
	}
	{
		err = p.Retire(context.Background(), f.org, id)
		requireReceiptCondition(t, errors.Is(err, ErrPackInUse), "published pack retired", err)
	}
	for _, q := range server.Requests() {
		requireReceiptCondition(t, q.Method != "GET" || q.IfMatch != "", "unbound cold read")
	}
	{
		_, err = p.Status(context.Background(), newID(t), id)
		requireReceiptCondition(t, errors.Is(err, ErrNotFound), "cross-organization pack access", err)
	}
}

func TestLostUploadAndPublicationReplies(t *testing.T) {
	f, p, _, objects := packing(t)
	r := accept(t, f.s, newID(t), f.envelope(t), "body")
	objects.create = func(ctx context.Context, key string, body io.ReadSeeker, n int64, d [32]byte) error {
		if err := objects.Store.Create(ctx, key, body, n, d); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	}
	p.afterPublish = func() error { return io.ErrUnexpectedEOF }
	id, err := p.Pack(context.Background(), f.org, []ID{r.ID})
	requireReceiptCondition(t, errors.Is(err, ErrUncertain), "lost publication reply not reported", err)
	{
		state, err := p.Status(context.Background(), f.org, id)
		requireReceiptCondition(t, err == nil, "published outcome not recoverable", state, err)
		requireReceiptCondition(t, state == "published", "published outcome not recoverable", state, err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_pack") == 1,
		"repeated PUT/publication created jobs",
	)
}

func TestUploadedUnpublishedPackCanBeRetired(t *testing.T) {
	f, p, server, _ := packing(t)
	r := accept(t, f.s, newID(t), f.envelope(t), "body")
	p.afterUpload = func(ID) error { return io.ErrUnexpectedEOF }
	id, err := p.Pack(context.Background(), f.org, []ID{r.ID})
	requireReceiptCondition(t, errors.Is(err, io.ErrUnexpectedEOF), err)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_receipt WHERE pack_id IS NOT NULL") == 0,
		"upload alone published locations",
	)
	key := objectKey(t, f, id)
	before, ok := server.Object(key)
	requireReceiptCondition(t, ok, "test did not upload body")
	requireReceiptCondition(t, len(before.Body) != 0, "test did not upload body")
	err = p.Retire(context.Background(), f.org, id)
	requireReceiptNoError(t, err)
	marker, _ := server.Object(key)
	requireReceiptCondition(t, marker.Retired, "retirement did not remove object body")
	requireReceiptCondition(t, len(marker.Body) == 0, "retirement did not remove object body")
	{
		state, err := p.Status(context.Background(), f.org, id)
		requireReceiptCondition(t, err == nil, state, err)
		requireReceiptCondition(t, state == "retired", state, err)
	}
	var out bytes.Buffer
	{
		err = f.s.Read(context.Background(), f.org, r.ID, 0, r.Size, &out)
		requireReceiptCondition(t, err == nil, "failed upload retirement lost accepted mail", err)
		requireReceiptCondition(t, out.String() == "body", "failed upload retirement lost accepted mail", err)
	}
}

func TestRetirementFencesDelayedCreate(t *testing.T) {
	f, p, server, objects := packing(t)
	r := accept(t, f.s, newID(t), f.envelope(t), "body")
	entered, release := make(chan string, 1), make(chan struct{})
	releaseGate := sync.OnceFunc(func() { close(release) })
	defer releaseGate()
	objects.create = func(ctx context.Context, key string, body io.ReadSeeker, n int64, d [32]byte) error {
		entered <- key
		<-release
		return objects.Store.Create(ctx, key, body, n, d)
	}
	type result struct {
		id  ID
		err error
	}
	done := make(chan result, 1)
	go func() { id, err := p.Pack(context.Background(), f.org, []ID{r.ID}); done <- result{id, err} }()
	var key string
	select {
	case key = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not start")
	}
	var textID string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(context.Background(), "SELECT id FROM fry_pack WHERE object_key=$1", key).Scan(&textID),
	)
	id, err := idFrom(textID)
	requireReceiptNoError(t, err)
	err = p.Retire(context.Background(), f.org, id)
	requireReceiptNoError(t, err)
	releaseGate()
	select {
	case result := <-done:
		requireReceiptCondition(t, result.err != nil, "retired uploader published")
	case <-time.After(5 * time.Second):
		t.Fatal("uploader stalled")
	}
	marker, _ := server.Object(key)
	requireReceiptCondition(t, marker.Retired, "late PUT resurrected original bytes")
	requireReceiptCondition(t, len(marker.Body) == 0, "late PUT resurrected original bytes")
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_receipt WHERE pack_id IS NOT NULL") == 0,
		"stale publication escaped",
	)
}

func TestPackRejectsLeaseOrManifestChange(t *testing.T) {
	for _, change := range []string{"lease", "epoch", "manifest", "references"} {
		t.Run(change, func(t *testing.T) {
			f, p, _, _ := packing(t)
			r := accept(t, f.s, newID(t), f.envelope(t), "body")
			p.afterUpload = func(id ID) error {
				switch change {
				case "lease":
					execSQL(
						t,
						f.pool,
						"UPDATE fry_pack SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1",
						id.String(),
					)
				case "epoch":
					execSQL(t, f.pool, "UPDATE fry_pack SET epoch=epoch+1 WHERE id=$1", id.String())
				case "manifest":
					execSQL(
						t,
						f.pool,
						"UPDATE fry_receipt SET manifest_version=manifest_version+1 WHERE id=$1",
						r.ID.String(),
					)
				case "references":
					execSQL(t, f.pool, "DELETE FROM fry_reference WHERE receipt=$1", r.ID.String())
				}
				return nil
			}
			id, err := p.Pack(context.Background(), f.org, []ID{r.ID})
			requireReceiptCondition(t, errors.Is(err, ErrStale), "stale pack published", err)
			requireReceiptCondition(
				t,
				scalar(t, f.pool, "SELECT count(*) FROM fry_receipt WHERE pack_id IS NOT NULL") == 0,
				"changed membership partially published",
			)
			err = p.Retire(context.Background(), f.org, id)
			requireReceiptNoError(t, err)
		})
	}
}

func TestOverlappingPackPublicationHasOneWinner(t *testing.T) {
	f, p, _, _ := packing(t)
	r := accept(t, f.s, newID(t), f.envelope(t), "body")
	// A second owner uses separate scratch space and the same database/members.
	secondStore, err := New(context.Background(), f.pool, f.members, f.s.db.opts)
	requireReceiptNoError(t, err)
	opts := p.opts
	opts.ScratchDirectory = t.TempDir()
	second, err := NewPacker(context.Background(), secondStore, p.objects, opts)
	requireReceiptNoError(t, err)
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	})
	uploaded := make(chan struct{}, 2)
	release := make(chan struct{})
	releaseGate := sync.OnceFunc(func() { close(release) })
	defer releaseGate()
	barrier := func(ID) error { uploaded <- struct{}{}; <-release; return nil }
	p.afterUpload = barrier
	second.afterUpload = barrier
	type result struct {
		id  ID
		err error
	}
	done := make(chan result, 2)
	for _, worker := range []*Packer{p, second} {
		go func(p *Packer) { id, err := p.Pack(context.Background(), f.org, []ID{r.ID}); done <- result{id, err} }(
			worker,
		)
	}
	for range 2 {
		select {
		case <-uploaded:
		case <-time.After(5 * time.Second):
			t.Fatal("pack build stalled")
		}
	}
	releaseGate()
	wins := 0
	for range 2 {
		result := <-done
		if result.err == nil {
			wins++
		} else {
			requireReceiptCondition(t, errors.Is(result.err, ErrStale), result.err)
			requireReceiptNoError(t, p.Retire(context.Background(), f.org, result.id))
		}
	}
	requireReceiptCondition(t, wins == 1, "multiple current packs", wins)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_pack WHERE state='published'") == 1,
		"multiple current packs",
		wins,
	)
}

func TestPackLivenessRecheckedAfterReceiptLock(t *testing.T) {
	f, p, _, _ := packing(t)
	r := accept(t, f.s, newID(t), f.envelope(t), "body")
	uploaded, release := make(chan struct{}), make(chan struct{})
	releaseGate := sync.OnceFunc(func() { close(release) })
	defer releaseGate()
	p.afterUpload = func(ID) error { close(uploaded); <-release; return nil }
	done := make(chan error, 1)
	go func() { _, err := p.Pack(context.Background(), f.org, []ID{r.ID}); done <- err }()
	wait(t, uploaded)
	tx, err := f.pool.Begin(context.Background())
	requireReceiptNoError(t, err)
	defer rollback(context.Background(), tx)
	_, err = tx.Exec(context.Background(), "SELECT id FROM fry_receipt WHERE id=$1 FOR UPDATE", r.ID.String())
	requireReceiptNoError(t, err)
	_, err = tx.Exec(context.Background(), "DELETE FROM fry_reference WHERE receipt=$1", r.ID.String())
	requireReceiptNoError(t, err)
	releaseGate()
	deadline := time.Now().Add(3 * time.Second)
	for scalar(t, f.pool, "SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND "+
		"wait_event_type='Lock'", f.pool.Config().ConnConfig.RuntimeParams["application_name"]) == 0 {
		requireReceiptCondition(t, !time.Now().After(deadline), "publisher did not block on receipt guard")
		time.Sleep(5 * time.Millisecond)
	}
	err = tx.Commit(context.Background())
	requireReceiptNoError(t, err)
	select {
	case err = <-done:
		requireReceiptCondition(t, errors.Is(err, ErrStale), "pre-lock reference snapshot published expired mail", err)
	case <-time.After(5 * time.Second):
		t.Fatal("publication stalled")
	}
}

func TestPackAdmissionAndChecksumMismatch(t *testing.T) {
	f, p, server, objects := packing(t)
	r := accept(t, f.s, newID(t), f.envelope(t), "body")
	{
		_, err := p.Pack(context.Background(), newID(t), []ID{r.ID})
		requireReceiptCondition(t, errors.Is(err, ErrNotFound), "cross-organization selection", err)
	}
	{
		_, err := p.Pack(context.Background(), f.org, []ID{r.ID, r.ID})
		requireReceiptCondition(t, errors.Is(err, ErrConflict), "duplicate receipt selected", err)
	}
	// A service returning a different immutable object for this key must not be
	// accepted just because the PUT response was ambiguous.
	objects.create = func(ctx context.Context, key string, _ io.ReadSeeker, _ int64, _ [32]byte) error {
		body := []byte("wrong image")
		digest := sha256.Sum256(body)
		if err := objects.Store.Create(ctx, key, bytes.NewReader(body), int64(len(body)), digest); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	}
	id, err := p.Pack(context.Background(), f.org, []ID{r.ID})
	requireReceiptCondition(t, errors.Is(err, objectstore.ErrIntegrity), "wrong object published", err)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, "SELECT count(*) FROM fry_receipt WHERE pack_id IS NOT NULL") == 0,
		"checksum mismatch exposed",
	)
	err = p.Retire(context.Background(), f.org, id)
	requireReceiptNoError(t, err)
	{
		object, _ := server.Object(objectKey(t, f, id))
		requireReceiptCondition(t, object.Retired, "bad upload not retired")
	}
	// Size admission happens before creating a pack job or uploading bytes.
	p.opts.MaxPackBytes = 1024
	large := accept(t, f.s, newID(t), f.envelope(t), strings.Repeat("x", 2048))
	{
		_, err = p.Pack(context.Background(), f.org, []ID{large.ID})
		requireReceiptCondition(t, errors.Is(err, hotstore.ErrTooLarge), "oversized pack admitted", err)
	}
}
