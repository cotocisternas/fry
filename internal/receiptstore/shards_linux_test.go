//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func shardFixture(t *testing.T) (*fixture, []IngestShard) {
	t.Helper()
	f := setup(t)
	ctx := context.Background()
	{
		_, err := f.pool.Exec(ctx, `DELETE FROM fry_ingest_shard; DELETE FROM fry_ingest_catalog`)
		requireReceiptCondition(t, err == nil, err)
	}
	second := [2]Member{}
	for i := range second {
		m, err := OpenLocalMember(
			context.Background(),
			f.pool,
			newID(t),
			[]string{"zone-a", "zone-b"}[i],
			t.TempDir(),
			hotOptions(),
		)
		requireReceiptNoError(t, err)
		second[i] = m
		t.Cleanup(func() {
			if err := m.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	shards := []IngestShard{
		{ID: newID(t), Members: [2]Member{f.members[0], f.members[1]}},
		{ID: newID(t), Members: second},
	}
	s, err := NewWithShards(ctx, f.pool, shards, f.s.db.opts)
	requireReceiptNoError(t, err)
	f.s = s
	return f, shards
}

func attemptFor(s *Store, index int) ID {
	for i := uint64(1); ; i++ {
		var id ID
		for n := range 8 {
			id[15-n] = byte((i >> uint(n*8)) & 0xff)
		}
		if placementIndex(id, len(s.shards)) == index {
			return id
		}
	}
}

func TestShardCatalogCanonicalAndConflict(t *testing.T) {
	f, shards := shardFixture(t)
	ctx := context.Background()
	reordered := []IngestShard{shards[1], shards[0]}
	reordered[0].Members[0], reordered[0].Members[1] = reordered[0].Members[1], reordered[0].Members[0]
	{
		_, err := NewWithShards(ctx, f.pool, reordered, f.s.db.opts)
		requireReceiptCondition(t, err == nil, "reorder", err)
	}
	for _, bad := range [][]IngestShard{
		shards[:1],
		{{ID: shards[0].ID, Members: shards[1].Members}, {ID: shards[1].ID, Members: shards[0].Members}},
		{{ID: shards[0].ID, Members: shards[0].Members}, {ID: shards[1].ID, Members: shards[0].Members}},
	} {
		{
			_, err := NewWithShards(ctx, f.pool, bad, f.s.db.opts)
			requireReceiptCondition(t, err != nil, "catalog change accepted")
		}
	}
}

func TestShardIndependentProgressAndAcceptedReplay(t *testing.T) {
	f, _ := shardFixture(t)
	ctx := context.Background()
	e := f.envelope(t)
	attempt := attemptFor(f.s, 1)
	receipt := accept(t, f.s, attempt, e, "independent bytes")
	blocked := receiptTestValue[*LocalMember](t, f.s.shards[0].Members[0])
	blocked.gate.Lock()
	defer blocked.gate.Unlock()
	var out bytes.Buffer
	{
		err := f.s.Read(ctx, f.org, receipt.ID, 0, receipt.Size, &out)
		requireReceiptCondition(t, err == nil, "unrelated gate blocked read", err)
		requireReceiptCondition(t, out.String() == "independent bytes", "unrelated gate blocked read", err)
	}
	fresh := newID(t)
	for placementIndex(fresh, 2) != 1 {
		fresh = newID(t)
	}
	deadline, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	{
		_, err := f.s.Accept(deadline, fresh, e, io.NopCloser(strings.NewReader("other shard")))
		requireReceiptCondition(t, err == nil, "unrelated gate blocked accept", err)
	}

	pending := newID(t)
	for placementIndex(pending, 2) != 1 {
		pending = newID(t)
	}
	normalized, data, receiptErr1 := normalize(e)
	requireReceiptNoError(t, receiptErr1)
	activePair := f.s.shards[1].Members
	{
		_, _, err := f.s.db.prepareMembers(
			ctx,
			pending,
			normalized,
			data,
			[2]MemberIdentity{activePair[0].identity(), activePair[1].identity()},
		)
		requireReceiptCondition(t, err == nil, err)
	}
	{
		err := f.s.Abort(deadline, f.org, pending)
		requireReceiptCondition(t, err == nil, "unrelated gate blocked abort", err)
	}
	requireAcceptedShardReplayUnderGates(t, f, deadline, attempt, e, receipt)
}

func TestShardSameInboxDistributionAndPersistentRetry(t *testing.T) {
	f, _ := shardFixture(t)
	ctx := context.Background()
	e := f.envelope(t)
	for i := range 32 {
		accept(t, f.s, ID{15: byte(i + 1)}, e, "same inbox")
	}
	var distinct int
	{
		err := f.pool.QueryRow(ctx, `SELECT count(DISTINCT ingest_shard) FROM fry_attempt`).Scan(&distinct)
		requireReceiptCondition(t, err == nil, "one inbox not distributed", distinct, err)
		requireReceiptCondition(t, distinct == 2, "one inbox not distributed", distinct, err)
	}
	attempt := newID(t)
	pair, err := f.s.pairForAttempt(ctx, attempt)
	requireReceiptNoError(t, err)
	normalized, data, err := normalize(e)
	requireReceiptNoError(t, err)
	claim, _, err := f.s.db.prepareMembers(
		ctx,
		attempt,
		normalized,
		data,
		[2]MemberIdentity{pair[0].identity(), pair[1].identity()},
	)
	requireReceiptNoError(t, err)
	_, err = f.pool.Exec(
		ctx,
		`UPDATE fry_attempt SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`,
		attempt.String(),
	)
	requireReceiptNoError(t, err)
	other := f.s.shards[0].Members
	if memberID(pair[0]) == memberID(other[0]) || memberID(pair[0]) == memberID(other[1]) {
		other = f.s.shards[1].Members
	}
	{
		_, _, err = f.s.db.prepareMembers(
			ctx,
			attempt,
			normalized,
			data,
			[2]MemberIdentity{other[0].identity(), other[1].identity()},
		)
		requireReceiptCondition(t, errors.Is(err, ErrConflict), "retry retargeted", err)
	}
	_, err = f.s.Accept(ctx, attempt, e, io.NopCloser(strings.NewReader("retry bytes")))
	requireReceiptNoError(t, err)
	var targets int
	{
		err = f.pool.QueryRow(ctx, `SELECT count(*) FROM fry_target WHERE attempt=$1`, claim.Attempt.String()).
			Scan(&targets)
		requireReceiptCondition(t, err == nil, "changed target count", targets, err)
		requireReceiptCondition(t, targets == 2, "changed target count", targets, err)
	}
}

func TestShardCatalogCannotAdoptUnidentifiedHistory(t *testing.T) {
	f, _ := shardFixture(t)
	accept(t, f.s, newID(t), f.envelope(t), "historical")
	{
		_, err := f.pool.Exec(context.Background(), `DELETE FROM fry_ingest_catalog`)
		requireReceiptCondition(t, err == nil, err)
	}
	{
		_, err := NewWithShards(context.Background(), f.pool, f.s.shards, f.s.db.opts)
		requireReceiptCondition(t, err != nil, "adopted unidentified history")
	}
}

func TestShardReadRechecksRewrittenLocators(t *testing.T) {
	f, _ := shardFixture(t)
	ctx := context.Background()
	e := f.envelope(t)
	attempt := attemptFor(f.s, 1)
	r := accept(t, f.s, attempt, e, "survivor body")
	dead := newID(t)
	for placementIndex(dead, len(f.s.shards)) != 1 {
		dead = newID(t)
	}
	expireReceipt(t, f, accept(t, f.s, dead, f.envelope(t), "dropped body"))
	targetFile := func(member ID) string {
		t.Helper()
		var file string
		requireReceiptNoError(t, f.pool.QueryRow(
			ctx,
			`SELECT incarnation FROM fry_target WHERE attempt=$1 AND member=$2`,
			attempt.String(),
			member.String(),
		).Scan(&file))
		return file
	}
	var attempts []ID
	var before string
	// Rewrite the member after the Store selected it and before it reads. A
	// fallback to the other replica would hide a stale locator, so exactly one
	// member attempt must serve the read.
	f.s.beforeReadAttempt = func(member ID) {
		attempts = append(attempts, member)
		if len(attempts) > 1 {
			return
		}
		before = targetFile(member)
		if err := f.s.ReclaimHot(ctx, member); err != nil {
			t.Error(err)
		}
	}
	var out bytes.Buffer
	{
		err := f.s.Read(ctx, f.org, r.ID, 0, r.Size, &out)
		requireReceiptCondition(t, err == nil, "stale selected locator", out.String(), err)
		requireReceiptCondition(t, out.String() == "survivor body", "stale selected locator", out.String(), err)
	}
	requireReceiptCondition(t, len(attempts) == 1, "rewritten member fell back to replica", attempts)
	requireReceiptCondition(t, targetFile(attempts[0]) != before, "reclaim did not rewrite selected target")
}

func TestShardPersistedPlacementOverridesHash(t *testing.T) {
	f, _ := shardFixture(t)
	ctx := context.Background()
	attempt := attemptFor(f.s, 0)
	e := f.envelope(t)
	normalized, data, err := normalize(e)
	requireReceiptNoError(t, err)
	original := f.s.shards[0].Members
	c, _, err := f.s.db.prepareMembers(
		ctx,
		attempt,
		normalized,
		data,
		[2]MemberIdentity{original[0].identity(), original[1].identity()},
	)
	requireReceiptNoError(t, err)
	replacement := f.s.shards[1]
	_, err = f.pool.Exec(ctx, `DELETE FROM fry_target WHERE attempt=$1`, attempt.String())
	requireReceiptNoError(t, err)
	_, err = f.pool.Exec(
		ctx,
		`UPDATE fry_attempt SET ingest_shard=$2,lease_until=clock_timestamp()-interval '1 `+
			`second' WHERE id=$1`,
		attempt.String(),
		replacement.ID.String(),
	)
	requireReceiptNoError(t, err)
	for _, m := range replacement.Members {
		v := m.identity()
		_, err = f.pool.Exec(
			ctx,
			`INSERT INTO fry_target(attempt,member,incarnation,failure_domain,owner_epoch) `+
				`VALUES($1,$2,$3,$4,$5)`,
			attempt.String(),
			v.Member.String(),
			v.Incarnation.String(),
			v.FailureDomain,
			v.OwnerEpoch,
		)
		requireReceiptNoError(t, err)
	}
	r := accept(t, f.s, attempt, e, "persisted selected pair")
	requireReceiptCondition(t, r.ID == c.Receipt, "retry created new receipt")
	var stored string
	{
		err = f.pool.QueryRow(ctx, `SELECT ingest_shard FROM fry_attempt WHERE id=$1`, attempt.String()).Scan(&stored)
		requireReceiptCondition(t, err == nil, "retry rehashed history", err)
		requireReceiptCondition(t, stored == replacement.ID.String(), "retry rehashed history", err)
	}
}

func TestShardConcurrentFirstBindingAndRowIntegrity(t *testing.T) {
	f, _ := shardFixture(t)
	ctx := context.Background()
	{
		_, err := f.pool.Exec(ctx, `DELETE FROM fry_ingest_shard; DELETE FROM fry_ingest_catalog`)
		requireReceiptCondition(t, err == nil, err)
	}
	catalogs := [][]IngestShard{append([]IngestShard(nil), f.s.shards...), append([]IngestShard(nil), f.s.shards...)}
	catalogs[1][0].ID = newID(t)
	ready := make(chan struct{})
	results := make(chan error, 2)
	for _, catalog := range catalogs {
		go func(c []IngestShard) { <-ready; _, err := NewWithShards(ctx, f.pool, c, f.s.db.opts); results <- err }(
			catalog,
		)
	}
	close(ready)
	wins := 0
	for range 2 {
		if err := <-results; err == nil {
			wins++
		} else {
			requireReceiptCondition(t, errors.Is(err, ErrConflict), err)
		}
	}
	requireReceiptConditionf(t, wins == 1, "first-bind winners %d", wins)
	var id, a, b string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(ctx, `SELECT id,member_a,member_b FROM fry_ingest_shard ORDER BY id LIMIT 1`).Scan(&id, &a, &b),
	)
	{
		_, err := f.pool.Exec(ctx, `UPDATE fry_ingest_shard SET member_b=$2 WHERE id=$1`, id, strings.Repeat("f", 32))
		requireReceiptCondition(t, err == nil, err)
	}
	for _, catalog := range catalogs {
		{
			_, err := NewWithShards(ctx, f.pool, catalog, f.s.db.opts)
			requireReceiptCondition(t, err != nil, "catalog JSON hid inconsistent rows")
		}
	}
}

func TestShardDuplicateValidationDoesNotAcquireGates(t *testing.T) {
	f, shards := shardFixture(t)
	member := receiptTestValue[*LocalMember](t, shards[0].Members[0])
	member.gate.Lock()
	defer member.gate.Unlock()
	shards[1].Members[0] = member
	done := make(chan error, 1)
	go func() { _, err := NewWithShards(context.Background(), f.pool, shards, f.s.db.opts); done <- err }()
	select {
	case err := <-done:
		requireReceiptCondition(t, err != nil, "duplicate accepted")
	case <-time.After(time.Second):
		t.Fatal("invalid catalog waited for gates")
	}
}

func TestShardAuthenticatedRemoteIsolationReadAbortAndReplay(t *testing.T) {
	f, _ := shardFixture(t)
	ctx := context.Background()
	s, calls, servers := remoteShardIsolationFixture(t, f)
	var err error
	e := f.envelope(t)
	a0, a1 := attemptFor(s, 0), attemptFor(s, 1)
	r0 := accept(t, s, a0, e, "offline replay")
	r1 := accept(t, s, a1, e, "remote other pair")
	for _, server := range servers[0] {
		err = server.Close(ctx)
		requireReceiptNoError(t, err)
	}
	before := calls[0].Load()
	again, err := s.Accept(ctx, a0, e, io.NopCloser(strings.NewReader("ignored")))
	requireReceiptCondition(t, err == nil, "offline accepted replay", err)
	requireReceiptCondition(t, again.ID == r0.ID, "offline accepted replay", err)
	var out bytes.Buffer
	{
		err = s.Read(ctx, f.org, r1.ID, 0, r1.Size, &out)
		requireReceiptCondition(t, err == nil, out.String(), err)
		requireReceiptCondition(t, out.String() == "remote other pair", out.String(), err)
	}
	pending := newID(t)
	for placementIndex(pending, 2) != 1 {
		pending = newID(t)
	}
	pair := s.shards[1].Members
	normalized, data, receiptErr2 := normalize(e)
	requireReceiptNoError(t, receiptErr2)
	_, _, err = s.db.prepareMembers(
		ctx,
		pending,
		normalized,
		data,
		[2]MemberIdentity{pair[0].identity(), pair[1].identity()},
	)
	requireReceiptNoError(t, err)
	{
		err = s.Abort(ctx, f.org, pending)
		requireReceiptCondition(t, err == nil, "remote targeted abort", err)
	}
	fresh := newID(t)
	for placementIndex(fresh, 2) != 1 {
		fresh = newID(t)
	}
	accept(t, s, fresh, e, "remote progress")
	requireReceiptCondition(t, calls[0].Load() == before, "unrelated remote pair contacted")
	{
		n := scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE attempt=$1 AND cancel_confirmed`,
			pending.String(),
		)
		requireReceiptCondition(t, n == 2, "targeted remote cancellation missing", n)
	}
}

func requireAcceptedShardReplayUnderGates(
	t *testing.T,
	f *fixture,
	deadline context.Context,
	attempt ID,
	e Envelope,
	receipt Receipt,
) {
	t.Helper()
	// Accepted retries use authority alone even with both selected gates held.
	pair := f.s.shards[1].Members
	receiptTestValue[*LocalMember](t, pair[0]).gate.Lock()
	receiptTestValue[*LocalMember](t, pair[1]).gate.Lock()
	defer func() {
		receiptTestValue[*LocalMember](t, pair[0]).gate.Unlock()
	}()
	defer func() {
		receiptTestValue[*LocalMember](t, pair[1]).gate.Unlock()
	}()
	again, err := f.s.Accept(deadline, attempt, e, io.NopCloser(strings.NewReader("ignored")))
	requireReceiptCondition(t, err == nil, "accepted replay waited for gate", err)
	requireReceiptCondition(t, again.ID == receipt.ID, "accepted replay waited for gate", err)
	wrongOrg := e
	wrongOrg.Organization = newID(t)
	{
		_, err = f.s.Accept(deadline, attempt, wrongOrg, io.NopCloser(strings.NewReader("ignored")))
		requireReceiptCondition(t, errors.Is(err, ErrConflict), "wrong organization replay", err)
	}
	wrong := e
	wrong.MailFrom = "different@example.test"
	{
		_, err = f.s.Accept(deadline, attempt, wrong, io.NopCloser(strings.NewReader("ignored")))
		requireReceiptCondition(t, errors.Is(err, ErrConflict), "changed accepted envelope", err)
	}
}

func remoteShardIsolationFixture(t *testing.T, f *fixture) (*Store, *[2]atomic.Int64, [2][2]*MemberServer) {
	t.Helper()
	ctx := context.Background()
	var calls [2]atomic.Int64
	var servers [2][2]*MemberServer
	remoteShards := make([]IngestShard, 2)
	for i, shard := range f.s.shards {
		memberFixture := *f
		memberFixture.members = [2]*LocalMember{
			receiptTestValue[*LocalMember](t, shard.Members[0]),
			receiptTestValue[*LocalMember](t, shard.Members[1]),
		}
		index := i
		members, srv := remoteFixture(t, &memberFixture, func(next http.Handler) http.Handler {
			return http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) { calls[index].Add(1); next.ServeHTTP(w, r) },
			)
		})
		servers[i] = srv
		remoteShards[i] = IngestShard{ID: shard.ID, Members: [2]Member{members[0], members[1]}}
	}
	s, err := NewWithShards(ctx, f.pool, remoteShards, f.s.db.opts)
	requireReceiptNoError(t, err)
	return s, &calls, servers
}
