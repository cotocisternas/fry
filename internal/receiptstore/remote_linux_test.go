//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
)

func remoteFixture(
	t *testing.T,
	f *fixture,
	wrappers ...func(http.Handler) http.Handler,
) ([2]*RemoteMember, [2]*MemberServer) {
	t.Helper()
	var remote [2]*RemoteMember
	var servers [2]*MemberServer
	for i := range remote {
		memberFixture := *f
		memberFixture.members[0] = f.members[i]
		server, certs, url := memberHTTP(t, &memberFixture, wrappers...)
		opts := RemoteMemberOptions{
			URL:             url,
			TLS:             receiptTestValue[*http.Transport](t, certs.allowed.Transport).TLSClientConfig,
			Member:          f.members[i].id,
			FailureDomain:   f.members[i].domain,
			ServerKey:       sha256.Sum256(certs.server.Certificates[0].Leaf.RawSubjectPublicKeyInfo),
			MaxMessageBytes: f.members[i].opts.MaxMessageBytes,
			MaxConnections:  8,
			Timeout:         30 * time.Second,
		}
		var err error
		remote[i], err = DialRemoteMember(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(remote[i].Close)
		servers[i] = server
	}
	return remote, servers
}

func TestRemoteReceiptAcceptanceReadAndUnavailableMember(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	members, servers := remoteFixture(t, f)
	s, err := NewWithMembers(ctx, f.pool, [2]Member{members[0], members[1]}, f.s.db.opts)
	requireReceiptNoError(t, err)
	envelope := f.envelope(t)
	attempt := newID(t)
	body := strings.Repeat("streamed business document\r\n", 12000)
	receipt := accept(t, s, attempt, envelope, body)
	if scalar(
		t,
		f.pool,
		`SELECT count(*) FROM fry_target WHERE attempt=$1 AND locator IS NOT NULL AND `+
			`owner_epoch>0`,
		attempt.String(),
	) != 2 {
		t.Fatal("missing owned copies")
	}
	var got bytes.Buffer
	if err = s.Read(ctx, f.org, receipt.ID, 0, receipt.Size, &got); err != nil || got.String() != body {
		t.Fatalf("read: %v, %d bytes", err, got.Len())
	}
	if scalar(t, f.pool, `SELECT count(*) FROM fry_member_read WHERE NOT closed`) != 0 ||
		scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin`) != 0 {
		t.Fatal("completed read left pins")
	}
	// A lost acknowledgment can be retried without retransmitting payload.
	replay := accept(t, s, attempt, envelope, "must not be consumed")
	requireReceiptCondition(t, replay.ID == receipt.ID, "duplicate receipt")
	err = servers[0].Close(ctx)
	requireReceiptNoError(t, err)
	got.Reset()
	if err = s.Read(ctx, f.org, receipt.ID, 0, receipt.Size, &got); err != nil || got.String() != body {
		t.Fatalf("surviving read: %v", err)
	}
	failed := newID(t)
	if _, err = s.Accept(
		ctx,
		failed,
		envelope,
		io.NopCloser(strings.NewReader("cannot acknowledge one copy")),
	); err == nil {
		t.Fatal("accepted with a stopped member")
	}
	if scalar(t, f.pool, `SELECT count(*) FROM fry_receipt WHERE attempt=$1`, failed.String()) != 0 {
		t.Fatal("published failed receipt")
	}
}

func TestRemoteClientRejectsWrongPeerAndPlacement(t *testing.T) {
	f := setup(t)
	_, certs, url := memberHTTP(t, f)
	opts := RemoteMemberOptions{
		URL:             url,
		TLS:             receiptTestValue[*http.Transport](t, certs.allowed.Transport).TLSClientConfig,
		Member:          f.members[0].id,
		FailureDomain:   f.members[0].domain,
		ServerKey:       sha256.Sum256(certs.server.Certificates[0].Leaf.RawSubjectPublicKeyInfo),
		MaxMessageBytes: 4 << 20,
		MaxConnections:  2,
		Timeout:         2 * time.Second,
	}
	wrong := opts
	wrong.ServerKey[0] ^= 1
	if _, err := DialRemoteMember(context.Background(), wrong); err == nil {
		t.Fatal("accepted wrong public key")
	}
	wrong = opts
	wrong.Member = newID(t)
	if _, err := DialRemoteMember(context.Background(), wrong); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong member: %v", err)
	}
	wrong = opts
	wrong.FailureDomain = "claimed-other-zone"
	if _, err := DialRemoteMember(context.Background(), wrong); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong placement: %v", err)
	}
}

func TestRemoteAbortClosesStreamsAndConfirmsBothMembers(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	members, _ := remoteFixture(t, f)
	s, err := NewWithMembers(ctx, f.pool, [2]Member{members[0], members[1]}, f.s.db.opts)
	requireReceiptNoError(t, err)
	attempt := newID(t)
	reader, writer := io.Pipe()
	defer func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	}()
	done := make(chan error, 1)
	go func() { _, e := s.Accept(ctx, attempt, f.envelope(t), reader); done <- e }()
	if _, err = io.WriteString(writer, strings.Repeat("partial", 10000)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for scalar(t, f.pool, `SELECT count(*) FROM fry_target WHERE attempt=$1`, attempt.String()) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("targets not registered")
		}
		time.Sleep(time.Millisecond)
	}
	err = s.Abort(ctx, f.org, attempt)
	requireReceiptNoError(t, err)
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("aborted stream accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("source stream remained blocked")
	}
	if scalar(
		t,
		f.pool,
		`SELECT count(*) FROM fry_target WHERE attempt=$1 AND cancel_confirmed`,
		attempt.String(),
	) != 2 {
		t.Fatal("missing actual closure proof")
	}
}

func TestRemoteReadDoesNotExposeFinalBytesWithoutClosureProof(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	members, _ := remoteFixture(t, f)
	s, err := NewWithMembers(ctx, f.pool, [2]Member{members[0], members[1]}, f.s.db.opts)
	requireReceiptNoError(t, err)
	receipt := accept(t, s, newID(t), f.envelope(t), "final bytes are not success")
	execSQL(
		t,
		f.pool,
		`CREATE FUNCTION fail_remote_closure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN `+
			`RAISE EXCEPTION 'fixture closure unavailable'; END $$`,
	)
	execSQL(
		t,
		f.pool,
		`CREATE TRIGGER fail_remote_closure BEFORE UPDATE ON fry_member_read FOR EACH ROW `+
			`EXECUTE FUNCTION fail_remote_closure()`,
	)
	var got bytes.Buffer
	err = s.Read(ctx, f.org, receipt.ID, 0, receipt.Size, &got)
	if err == nil || got.Len() != 0 {
		t.Fatalf("unverified final bytes exposed: %q, %v", got.String(), err)
	}
	if scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin WHERE owner_member IS NOT NULL`) != 2 {
		t.Fatal("lost read closure debt")
	}
	if scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin WHERE owner_member IS NULL`) != 0 {
		t.Fatal("coordinator pin remained after its own handle closed")
	}
}

func TestRemoteLostWriteResponseCanRetrySameAttempt(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	wrapper := loseCompletedMemberWriteResponse()
	members, _ := remoteFixture(t, f, wrapper)
	s, err := NewWithMembers(ctx, f.pool, [2]Member{members[0], members[1]}, f.s.db.opts)
	requireReceiptNoError(t, err)
	attempt := newID(t)
	envelope := f.envelope(t)
	if _, err = s.Accept(ctx, attempt, envelope, io.NopCloser(strings.NewReader("stable content"))); err == nil {
		t.Fatal("lost response reported success")
	}
	if scalar(
		t,
		f.pool,
		`SELECT count(*) FROM fry_target WHERE attempt=$1 AND locator IS NOT NULL`,
		attempt.String(),
	) != 2 {
		t.Fatal("fixture did not finish both durable writes before losing reply")
	}
	if scalar(t, f.pool, `SELECT count(*) FROM fry_receipt WHERE attempt=$1`, attempt.String()) != 0 {
		t.Fatal("failed Accept published receipt")
	}
	expire(t, f, attempt)
	receipt := accept(t, s, attempt, envelope, "stable content")
	if receipt.Size != int64(len("stable content")) ||
		scalar(t, f.pool, `SELECT count(*) FROM fry_receipt WHERE attempt=$1`, attempt.String()) != 1 {
		t.Fatal("retry changed identity or duplicated receipt")
	}
	for _, member := range f.members {
		if len(member.bytes.Inventory()) != 1 {
			t.Fatal("retry duplicated hot byte records")
		}
	}
}

func loseCompletedMemberWriteResponse() func(http.Handler) http.Handler {
	var lose atomic.Bool
	lose.Store(true)
	var durableWrites sync.WaitGroup
	durableWrites.Add(2)
	return func(original http.Handler) http.Handler {
		var initial atomic.Bool
		initial.Store(true)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			original.ServeHTTP(w, r)
			if r.URL.Path == "/v1/write" && initial.CompareAndSwap(true, false) {
				// A lost reply proves neither sibling cancellation nor storage closure.
				// Finish both original writes before losing either reply so retry tests
				// completed durable copies rather than racing a still-running writer.
				durableWrites.Done()
				durableWrites.Wait()
				if lose.CompareAndSwap(true, false) {
					panic(http.ErrAbortHandler)
				}
			}
		})
	}
}

// A generated source and hash sink keep the test itself from buffering the body.
type replicaPattern struct {
	remaining, offset int64
	largest           int
}

func (r *replicaPattern) Read(p []byte) (int, error) {
	r.largest = max(r.largest, len(p))
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if r.remaining < 0 || r.offset < 0 || r.offset > math.MaxInt64-int64(len(p)) {
		return 0, errors.New("invalid fixture bounds")
	}
	n := int(min(int64(len(p)), r.remaining))
	for i := range p[:n] {
		p[i] = byte((r.offset + int64(i)) % 251)
	}
	r.offset += int64(n)
	r.remaining -= int64(n)
	return n, nil
}

func resizeRemoteFixture(t *testing.T, f *fixture, size int64) {
	t.Helper()
	for i, m := range f.members {
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		next, err := OpenLocalMember(
			context.Background(),
			f.pool,
			m.id,
			m.domain,
			f.dirs[i],
			hotstore.Options{MaxMessageBytes: size, MaxBytes: 1 << 30, MaxConcurrentAppends: 2},
		)
		if err != nil {
			t.Fatal(err)
		}
		f.members[i] = next
		t.Cleanup(func() {
			if err := next.Close(); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestRemoteLargeMessageUsesBoundedStreaming(t *testing.T) {
	if testing.Short() {
		t.Skip("200 MiB remote stream")
	}
	f := setup(t)
	ctx := context.Background()
	const size = int64(200 << 20)
	resizeRemoteFixture(t, f, size)
	members, _ := remoteFixture(t, f)
	s, err := NewWithMembers(ctx, f.pool, [2]Member{members[0], members[1]}, f.s.db.opts)
	requireReceiptNoError(t, err)
	source := &replicaPattern{remaining: size}
	expected := sha256.New()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	receipt, err := s.Accept(ctx, newID(t), f.envelope(t), io.NopCloser(io.TeeReader(source, expected)))
	requireReceiptNoError(t, err)
	got := sha256.New()
	err = s.Read(ctx, f.org, receipt.ID, 0, size, got)
	requireReceiptNoError(t, err)
	runtime.ReadMemStats(&after)
	if receipt.Size != size || !bytes.Equal(receipt.SHA256[:], expected.Sum(nil)) ||
		!bytes.Equal(got.Sum(nil), expected.Sum(nil)) ||
		source.largest > 64<<10 {
		t.Fatal("large stream differs or source request is unbounded")
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > 100<<20 {
		t.Fatalf("large stream allocated %d bytes, expected less than half a body", allocated)
	}
	t.Logf(
		"200 MiB accepted across two TLS members and read: elapsed=%s, total Go allocation=%d "+
			"bytes",
		time.Since(started),
		allocated,
	)
}
