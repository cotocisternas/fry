//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func remoteMaintenance(
	t *testing.T,
	f *fixture,
	wrappers ...func(http.Handler) http.Handler,
) (*Store, [2]*RemoteMember, [2]*MemberServer) {
	t.Helper()
	members, servers := remoteFixture(t, f, wrappers...)
	s, err := NewWithMembers(context.Background(), f.pool, [2]Member{members[0], members[1]}, f.s.db.opts)
	requireReceiptNoError(t, err)
	return s, members, servers
}

func cachedCoordinator(t *testing.T, f *fixture, members [2]*RemoteMember) *Store {
	t.Helper()
	var cached [2]Member
	for i, m := range members {
		cached[i] = &RemoteMember{
			descriptor: m.identity(),
			endpoint:   m.endpoint,
			client:     m.client,
			transport:  m.transport,
			maxBytes:   m.maxBytes,
			timeout:    m.timeout,
		}
	}
	s, err := NewWithMembers(context.Background(), f.pool, cached, f.s.db.opts)
	requireReceiptNoError(t, err)
	return s
}

func TestRemoteMaintenanceRefreshesIndependentCoordinators(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s, members, _ := remoteMaintenance(t, f)
	second := cachedCoordinator(t, f, members)
	dead := accept(t, s, newID(t), f.envelope(t), "dead original")
	live := accept(t, s, newID(t), f.envelope(t), "live original")
	expireReceipt(t, f, dead)
	before := [2]ID{members[0].identity().Incarnation, members[1].identity().Incarnation}
	for _, m := range members {
		requireReceiptNoError(t, s.ReclaimHot(ctx, m.identity().Member))
	}
	for i, m := range second.members {
		requireReceiptCondition(t, m.identity().Incarnation == before[i], "test lost stale cache")
	}
	var got bytes.Buffer
	{
		err := second.Read(ctx, f.org, live.ID, 0, live.Size, &got)
		requireReceiptCondition(t, err == nil, "stale coordinator could not read rewritten receipt", got.String(), err)
		requireReceiptCondition(
			t,
			got.String() == "live original",
			"stale coordinator could not read rewritten receipt",
			got.String(),
			err,
		)
	}
	accept(t, second, newID(t), f.envelope(t), "arrived after rewrite")
	for i, m := range second.members {
		requireReceiptCondition(
			t,
			m.identity().Incarnation != before[i],
			"prepare did not refresh physical incarnation",
		)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member WHERE maintenance_job IS NOT NULL`) == 0,
		"finished operation left gate",
	)
	{
		n, err := s.FinishCleanup(ctx, 100)
		requireReceiptCondition(t, err == nil, "remote dead copies not accounted", n, err)
		requireReceiptCondition(t, n == 1, "remote dead copies not accounted", n, err)
	}
}

func TestRemoteMaintenanceGateExcludesPreparedTargets(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s, members, _ := remoteMaintenance(t, f)
	dead := accept(t, s, newID(t), f.envelope(t), "dead")
	accept(t, s, newID(t), f.envelope(t), "live")
	expireReceipt(t, f, dead)
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	f.members[0].rewriteFault = func(point string) error {
		if point == "after_copy" {
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- s.ReclaimHot(ctx, members[0].identity().Member) }()
	<-entered
	env, data, err := normalize(f.envelope(t))
	requireReceiptNoError(t, err)
	attempt := newID(t)
	{
		_, _, err = s.db.prepareMembers(
			ctx,
			attempt,
			env,
			data,
			[2]MemberIdentity{members[0].identity(), members[1].identity()},
		)
		requireReceiptCondition(t, errors.Is(err, ErrBusy), "prepare crossed committed snapshot gate", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_target WHERE attempt=$1`, attempt.String()) == 0,
		"unscanned target registered",
	)
	unblock()
	err = <-done
	requireReceiptNoError(t, err)
}

func TestRemoteMaintenancePreparingAttemptKeepsBytesAndReleasesGate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s, members, _ := remoteMaintenance(t, f)
	env, data, err := normalize(f.envelope(t))
	requireReceiptNoError(t, err)
	attempt := newID(t)
	_, _, err = s.db.prepareMembers(
		ctx,
		attempt,
		env,
		data,
		[2]MemberIdentity{members[0].identity(), members[1].identity()},
	)
	requireReceiptNoError(t, err)
	{
		err = s.ReclaimHot(ctx, members[0].identity().Member)
		requireReceiptCondition(t, err != nil, "preparing attempt allowed snapshot")
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member WHERE maintenance_job IS NOT NULL`) == 0,
		"known busy snapshot retained gate",
	)
	err = s.Abort(ctx, f.org, attempt)
	requireReceiptNoError(t, err)
	accept(t, s, newID(t), f.envelope(t), "admission restored")
}

func TestRemoteMaintenanceLostReplyRetriesSameOperation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var lose atomic.Bool
	lose.Store(true)
	wrapper := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/reclaim" && lose.CompareAndSwap(true, false) {
				buffer := &discardMemberResponse{header: http.Header{}}
				next.ServeHTTP(buffer, r)
				connection, _, err := http.NewResponseController(w).Hijack()
				if err == nil {
					if err := connection.Close(); err != nil {
						t.Error(err)
					}
				}
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	s, members, _ := remoteMaintenance(t, f, wrapper)
	dead := accept(t, s, newID(t), f.envelope(t), "dead")
	accept(t, s, newID(t), f.envelope(t), "live")
	expireReceipt(t, f, dead)
	operation := newID(t)
	old := members[0].identity().Incarnation
	{
		err := members[0].ReclaimHot(ctx, operation)
		requireReceiptCondition(t, err != nil, "lost reply acknowledged")
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_maintenance WHERE id=$1 AND state='complete'`,
			operation.String(),
		) == 1,
		"operation outcome not durable",
	)
	{
		state, err := members[0].MaintenanceStatus(ctx, operation)
		requireReceiptCondition(t, err == nil, "lost response status", state, err)
		requireReceiptCondition(t, state == "complete", "lost response status", state, err)
	}
	{
		err := members[0].ReclaimHot(ctx, operation)
		requireReceiptCondition(t, err == nil, "same operation retry failed", err)
	}
	requireReceiptCondition(t, members[0].identity().Incarnation != old, "retry did not refresh rewritten identity")
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_hot_rewrite WHERE member=$1`,
			members[0].identity().Member.String(),
		) == 1,
		"retry rewrote twice",
	)
}

type discardMemberResponse struct{ header http.Header }

func (w *discardMemberResponse) Header() http.Header         { return w.header }
func (w *discardMemberResponse) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardMemberResponse) WriteHeader(int)             {}

func TestRemoteMaintenanceUncertainPublicationRequiresCleanRecovery(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s, members, servers := remoteMaintenance(t, f)
	dead := accept(t, s, newID(t), f.envelope(t), "dead")
	live := accept(t, s, newID(t), f.envelope(t), "live")
	expireReceipt(t, f, dead)
	m := f.members[0]
	old := members[0].identity().Incarnation
	m.rewriteFault = func(point string) error {
		if point == "after_publish" {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	{
		err := s.ReclaimHot(ctx, m.id)
		requireReceiptCondition(t, err != nil, "uncertain publication acknowledged")
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member WHERE id=$1 AND maintenance_job IS NOT NULL`,
			m.id.String(),
		) == 1,
		"uncertain publication released gate",
	)
	{
		_, err := os.Stat(filepath.Join(f.dirs[0], "data.fry"))
		requireReceiptCondition(t, err == nil, "uncertain publication deleted old file", err)
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_hot_rewrite WHERE member=$1 AND state='published'`,
			m.id.String(),
		) == 1,
		"rewrite tracking lost",
	)
	// Even after the process's physical lock disappears, an unclosed owner
	// forbids startup and filesystem reconciliation.
	requireReceiptNoError(t, m.Close())
	{
		_, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, f.dirs[0], hotOptions())
		requireReceiptCondition(t, errors.Is(err, ErrBusy), "unclosed owner takeover", err)
	}
	requireReceiptNoError(t, servers[0].Close(ctx))
	next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, f.dirs[0], hotOptions())
	requireReceiptNoError(t, err)
	t.Cleanup(func() {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
	})
	requireCleanRemoteRewriteRecovery(t, f, m, next, old, live)
}

func TestRemoteMaintenanceDrainsReadAndPreservesParentPin(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s, members, servers := remoteMaintenance(t, f)
	pinned := accept(t, s, newID(t), f.envelope(t), "pinned original")
	dead := accept(t, s, newID(t), f.envelope(t), "dead original")
	expireReceipt(t, f, dead)
	parent, err := s.Open(ctx, f.org, pinned.ID, 0, pinned.Size)
	requireReceiptNoError(t, err)
	defer func() {
		if err := parent.Close(); err != nil {
			t.Error(err)
		}
	}()
	var parentID string
	err = f.pool.QueryRow(
		ctx,
		`SELECT id FROM fry_read_pin WHERE receipt=$1 AND owner_member IS NULL`,
		pinned.ID.String(),
	).
		Scan(&parentID)
	requireReceiptNoError(t, err)
	pin, err := idFrom(parentID)
	requireReceiptNoError(t, err)
	before := members[0].identity().Incarnation
	writer := &heldReadWriter{entered: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(writer.release) })
	defer release()
	readDone := make(chan error, 1)
	go func() {
		readDone <- servers[0].owner.read(ctx, newID(t), pin, f.org, pinned.ID, before, 0, pinned.Size, writer)
	}()
	select {
	case <-writer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("read did not start")
	}
	expireReceipt(t, f, pinned)
	rewriteDone := make(chan error, 1)
	go func() { rewriteDone <- s.ReclaimHot(ctx, members[0].identity().Member) }()
	select {
	case err := <-rewriteDone:
		t.Fatal("rewrite did not drain actual read", err)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	err = <-readDone
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, writer.String() == "pinned original", "active read damaged")
	err = <-rewriteDone
	requireReceiptNoError(t, err)
	requireRemoteParentPinAfterRewrite(t, f, servers[0], pin, pinned, before)
}

func TestRemoteAcceptedRetryDoesNotRequireIdentityRefresh(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s, _, servers := remoteMaintenance(t, f)
	attempt := newID(t)
	env := f.envelope(t)
	original := accept(t, s, attempt, env, "original")
	for _, server := range servers {
		requireReceiptNoError(t, server.Close(ctx))
	}
	source := &unreadSource{}
	retry, err := s.Accept(ctx, attempt, env, source)
	requireReceiptCondition(t, err == nil, "accepted retry depended on stopped member", retry, source, err)
	requireReceiptCondition(t, retry.ID == original.ID, "accepted retry depended on stopped member", retry, source, err)
	requireReceiptCondition(t, source.reads == 0, "accepted retry depended on stopped member", retry, source, err)
	requireReceiptCondition(t, source.closed, "accepted retry depended on stopped member", retry, source, err)
	changed := env
	changed.MailFrom = "different@example.test"
	{
		_, err := s.Accept(ctx, attempt, changed, &unreadSource{})
		requireReceiptCondition(t, errors.Is(err, ErrConflict), "stopped members weakened envelope binding", err)
	}
}

func TestRemoteMaintenanceSerializesDelayedIdentityResponses(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var delay atomic.Bool
	captured, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	wrapper := delayMemberIdentityResponse(t, &delay, captured, release)
	s, members, _ := remoteMaintenance(t, f, wrapper)
	second := cachedCoordinator(t, f, members)
	dead := accept(t, s, newID(t), f.envelope(t), "dead")
	accept(t, s, newID(t), f.envelope(t), "live")
	expireReceipt(t, f, dead)
	old := members[0].identity().Incarnation
	delay.Store(true)
	oldRefresh := make(chan error, 1)
	go func() { oldRefresh <- members[0].refresh(ctx) }()
	select {
	case <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("old identity response not captured")
	}
	requireReceiptNoError(t, second.ReclaimHot(ctx, members[0].identity().Member))
	accepted := make(chan error, 1)
	attempt, env := newID(t), f.envelope(t)
	go func() {
		_, err := s.Accept(ctx, attempt, env, io.NopCloser(bytes.NewBufferString("after rewrite")))
		accepted <- err
	}()
	// The second refresh must wait for the older response, so that a late old
	// reply cannot overwrite the incarnation used by a prepared write.
	select {
	case err := <-accepted:
		t.Fatal("new refresh overtook pending identity response", err)
	case <-time.After(30 * time.Millisecond):
	}
	unblock()
	requireReceiptNoError(t, <-oldRefresh)
	{
		err := <-accepted
		requireReceiptCondition(t, err == nil, "valid prepared write used stale cached incarnation", err)
	}
	requireReceiptCondition(
		t,
		members[0].identity().Incarnation != old,
		"late identity response regressed physical incarnation",
	)
}

func requireRemoteParentPinAfterRewrite(
	t *testing.T,
	f *fixture,
	server *MemberServer,
	pin ID,
	pinned Receipt,
	before ID,
) {
	t.Helper()
	ctx := context.Background()
	var err error
	// A stale incarnation is deliberately sent: owner/receipt/pin authority
	// resolves the new locator while the unchanged parent pin protects it.
	var got bytes.Buffer
	{
		err = server.owner.read(ctx, newID(t), pin, f.org, pinned.ID, before, 0, pinned.Size, &got)
		requireReceiptCondition(t, err == nil, "parent pin lost across rewrite", got.String(), err)
		requireReceiptCondition(
			t,
			got.String() == "pinned original",
			"parent pin lost across rewrite",
			got.String(),
			err,
		)
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_read_pin WHERE receipt=$1 AND owner_member IS NOT NULL`,
			pinned.ID.String(),
		) == 0,
		"completed reads left member pins",
	)
}

func requireCleanRemoteRewriteRecovery(t *testing.T, f *fixture, m, next *LocalMember, old ID, live Receipt) {
	t.Helper()
	requireReceiptCondition(t, next.bytes.FileID() != old, "recovery selected unpublished generation")
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member WHERE id=$1 AND maintenance_job IS NOT NULL`,
			m.id.String(),
		) == 0,
		"clean recovery retained stale gate",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_hot_rewrite WHERE member=$1 AND state IN `+
				`('building','published')`,
			m.id.String(),
		) == 0,
		"clean recovery left tracked file debt",
	)
	requireReceiptCondition(t, len(next.bytes.Inventory()) == 1, "recovered live bytes missing")
	requireReceiptCondition(t, next.bytes.Inventory()[0].Key.Receipt == live.ID, "recovered live bytes missing")
}

func delayMemberIdentityResponse(
	t *testing.T,
	delay *atomic.Bool,
	captured chan<- struct{},
	release <-chan struct{},
) func(http.Handler) http.Handler {
	t.Helper()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/identity" && delay.CompareAndSwap(true, false) {
				// Capture an authoritative old incarnation, but delay its network reply
				// until another coordinator has completed a physical rewrite.
				recorder := httptest.NewRecorder()
				next.ServeHTTP(recorder, r)
				close(captured)
				<-release
				for key, values := range recorder.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(recorder.Code)
				if _, err := w.Write(recorder.Body.Bytes()); err != nil {
					t.Error(err)
				}
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
