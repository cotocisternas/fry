//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"

	"github.com/jackc/pgx/v5"
)

func twoCapacityLaneOptions(records int64, extraByte int64) hotstore.Options {
	opts := capacityOptions(records)
	e := encodedTestBytes(opts.MaxMessageBytes)
	opts.MaxFileBytes = 64 + 16*e + extraByte
	opts.MaxBytes = 2*opts.MaxFileBytes + 4*e
	return opts
}

func capacityAssignedLane(t *testing.T, f *fixture, c claim, m *LocalMember) int16 {
	t.Helper()
	var lane int16
	requireReceiptNoError(
		t,
		f.pool.QueryRow(
			context.Background(),
			`SELECT capacity_lane FROM fry_target WHERE attempt=$1 AND member=$2`,
			c.Attempt.String(),
			m.id.String(),
		).
			Scan(&lane),
	)
	return lane
}

func waitCapacityBlock(ctx context.Context, f *fixture, pid uint32) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := f.pool.QueryRow(
			ctx,
			`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1::int=ANY(pg_blocking_pids(pid)))`,
			pid,
		).
			Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func capacityReadTarget(t *testing.T, f *fixture, c claim, m *LocalMember, payload []byte) {
	t.Helper()
	var raw []byte
	var settled bool
	requireReceiptNoError(
		t,
		f.pool.QueryRow(
			context.Background(),
			`SELECT locator,capacity_settled FROM fry_target WHERE attempt=$1 AND member=$2`,
			c.Attempt.String(),
			m.id.String(),
		).
			Scan(&raw, &settled),
	)
	requireReceiptCondition(t, settled, "copy not settled")
	var loc hotstore.Locator
	requireReceiptNoError(t, json.Unmarshal(raw, &loc))
	var out bytes.Buffer
	{
		err := m.bytes.Read(context.Background(), loc, 0, int64(len(payload)), &out)
		requireReceiptCondition(t, err == nil, "direct member original differs", err)
		requireReceiptCondition(t, bytes.Equal(out.Bytes(), payload), "direct member original differs", err)
	}
}

func TestMemberCapacityDifferentLaneRefundProgressesWhileSameLaneWaits(t *testing.T) {
	opts := twoCapacityLaneOptions(2, 0)
	f := setupWithPoolConfig(t, nil, opts, Options{Lease: 15 * time.Second, RecoveryRetention: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first := prepare(t, f, newID(t), f.envelope(t))
	second := prepare(t, f, newID(t), f.envelope(t))
	m := f.members[0]
	laneA, laneB := capacityAssignedLane(t, f, first, m), capacityAssignedLane(t, f, second, m)
	requireReceiptCondition(t, laneA != laneB, "free other lane was stranded after first record slot filled")
	locker, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer rollback(context.Background(), locker)
	_, err = locker.Exec(
		ctx,
		`UPDATE fry_member_file_lane SET reserved_bytes=reserved_bytes WHERE member=$1 AND `+
			`file_id=$2 AND lane=$3`,
		m.id.String(),
		m.bytes.FileID().String(),
		laneA,
	)
	requireReceiptNoError(t, err)
	payloadA, payloadB := []byte("short first original"), []byte("short independent original")
	doneA, doneB := make(chan error, 1), make(chan error, 1)
	go func() { doneA <- m.write(ctx, f.s.db, first, io.NopCloser(bytes.NewReader(payloadA))) }()
	waitSameLaneRefundBlocked(t, ctx, cancel, f, locker, doneA)
	// Start the other refund only after the first writer is actually blocked.
	// This also catches a residual common file lock held across the lane wait.
	go func() { doneB <- m.write(ctx, f.s.db, second, io.NopCloser(bytes.NewReader(payloadB))) }()
	releaseCtx, releaseCancel := awaitDifferentLaneRefund(t, cancel, locker, doneA, doneB)
	capacityReadTarget(t, f, second, m, payloadB)
	requireSameLaneTargetUnsettled(t, ctx, cancel, f, first, m, locker, releaseCtx, releaseCancel, doneA)
	releaseSameLaneRefund(t, cancel, locker, releaseCtx, releaseCancel, doneA)
	capacityReadTarget(t, f, first, m, payloadA)
	requireCapacityLaneRefundCounters(t, f, m, first, second, laneA, laneB, payloadA, payloadB)
}

func TestMemberCapacityLaneCeilingsDoNotRoundAggregateUp(t *testing.T) {
	opts := twoCapacityLaneOptions(3, 1)
	f := setupWithPoolConfig(t, nil, opts, Options{Lease: 10 * time.Second, RecoveryRetention: time.Hour})
	ctx := context.Background()
	for _, m := range f.members {
		var lanes int
		var records, bytes int64
		requireReceiptNoError(
			t,
			f.pool.QueryRow(ctx, `SELECT count(*),sum(record_ceiling)::bigint,sum(byte_ceiling)::bigint FROM `+
				`fry_member_file_lane WHERE member=$1 AND file_id=$2`, m.id.String(), m.bytes.FileID().String()).
				Scan(&lanes, &records, &bytes),
		)
		requireReceiptCondition(t, lanes == 2, "partition lost or invented aggregate capacity", lanes, records, bytes)
		requireReceiptCondition(
			t,
			records == opts.MaxRecords,
			"partition lost or invented aggregate capacity",
			lanes,
			records,
			bytes,
		)
		requireReceiptCondition(
			t,
			bytes == opts.MaxFileBytes-64,
			"partition lost or invented aggregate capacity",
			lanes,
			records,
			bytes,
		)
	}
	// Record ceilings are 2+1. Filtered fallback must fill all three real slots,
	// regardless of the deterministic preferred lane for these actual attempts.
	claims := []claim{}
	for range 3 {
		claims = append(claims, prepare(t, f, newID(t), f.envelope(t)))
	}
	{
		_, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "over aggregate count")
		requireReceiptCondition(
			t,
			errors.Is(err, hotstore.ErrCapacity),
			"rounded lane ceilings overbooked record count",
		)
	}
	for _, c := range claims {
		requireReceiptNoError(t, f.s.Abort(ctx, f.org, c.Attempt))
	}
	for _, m := range f.members {
		requireReceiptNoError(t, f.s.ReclaimHot(ctx, m.id))
		n, b, _ := capacityCounters(t, f, m)
		requireReceiptCondition(t, n == 0, "census left stranded lane reservation", n, b)
		requireReceiptCondition(t, b == 64, "census left stranded lane reservation", n, b)
	}
}

func TestMemberCapacityPreparedLaneSurvivesCleanReopenAndRetry(t *testing.T) {
	opts := twoCapacityLaneOptions(2, 0)
	f := setupWithPoolConfig(t, nil, opts, Options{Lease: 10 * time.Second, RecoveryRetention: time.Hour})
	ctx := context.Background()
	envelope := f.envelope(t)
	c := prepare(t, f, newID(t), envelope)
	prepare(t, f, newID(t), f.envelope(t))
	var lanes [2]int16
	for i, m := range f.members {
		lanes[i] = capacityAssignedLane(t, f, c, m)
		requireReceiptNoError(t, m.Close())
	}
	for i, m := range f.members {
		next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, opts)
		requireReceiptNoError(t, err)
		defer func() {
			if err := next.Close(); err != nil {
				t.Error(err)
			}
		}()
		f.members[i] = next
	}
	var err error
	f.s, err = New(ctx, f.pool, f.members, f.s.db.opts)
	requireReceiptNoError(t, err)
	expire(t, f, c.Attempt)
	r, err := capacityAccept(ctx, f, c.Attempt, envelope, "retry with retained lane identity")
	requireReceiptNoError(t, err)
	for i, m := range f.members {
		requireReceiptCondition(t, capacityAssignedLane(t, f, c, m) == lanes[i], "prepared retry moved reservation")
		n, _, _ := capacityCounters(t, f, m)
		requireReceiptCondition(t, n == 2, "prepared retry recharged aggregate records", n)
	}
	replay, err := capacityAccept(ctx, f, c.Attempt, envelope, "ignored accepted source")
	requireReceiptCondition(t, err == nil, "accepted lane replay changed identity", err)
	requireReceiptCondition(t, replay == r, "accepted lane replay changed identity", err)
}

func TestMemberCapacityLaneVersionAndCeilingsRefuseBeforeFilesystemMutation(t *testing.T) {
	for _, damage := range []string{"count", "record-ceiling", "byte-ceiling"} {
		t.Run(damage, func(t *testing.T) {
			opts := twoCapacityLaneOptions(3, 1)
			f := setupWithPoolConfig(t, nil, opts, Options{Lease: 10 * time.Second, RecoveryRetention: time.Hour})
			m := f.members[0]
			requireReceiptNoError(t, m.Close())
			ctx := context.Background()
			var err error
			switch damage {
			case "count":
				_, err = f.pool.Exec(ctx, `UPDATE fry_member_profile SET lane_count=3 WHERE member=$1`, m.id.String())
			case "record-ceiling":
				_, err = f.pool.Exec(
					ctx,
					`UPDATE fry_member_file_lane SET record_ceiling=record_ceiling+1 WHERE member=$1 AND `+
						`lane=0`,
					m.id.String(),
				)
			case "byte-ceiling":
				_, err = f.pool.Exec(
					ctx,
					`UPDATE fry_member_file_lane SET byte_ceiling=byte_ceiling+1 WHERE member=$1 AND lane=0`,
					m.id.String(),
				)
			}
			requireReceiptNoError(t, err)
			err = os.WriteFile(filepath.Join(m.root, "stage-lane-profile-sentinel"), []byte("preserve"), 0o600)
			requireReceiptNoError(t, err)
			before := memberFilesTree(t, m.root)
			next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, opts)
			if err == nil {
				if err := next.Close(); err != nil {
					t.Error(err)
				}
				t.Fatal("incompatible lane profile or invented quota accepted", damage)
			}
			memberFilesUnchanged(t, m.root, before)
		})
	}
}

// The SQL fault suppresses a real guarded debit, reproducing the candidate
// disappearing after selection without fabricating an accepted receipt.
func TestMemberCapacityCandidateLossRollsBackBothMembers(t *testing.T) {
	for _, second := range []bool{false, true} {
		t.Run(fmt.Sprint("second=", second), func(t *testing.T) {
			f := setupWithPoolConfig(
				t,
				nil,
				twoCapacityLaneOptions(2, 0),
				Options{Lease: 10 * time.Second, RecoveryRetention: time.Hour},
			)
			ctx := context.Background()
			members := append([]*LocalMember(nil), f.members[:]...)
			sort.Slice(members, func(i, j int) bool { return members[i].id.String() < members[j].id.String() })
			selected := members[0]
			if second {
				selected = members[1]
			}
			_, err := f.pool.Exec(
				ctx,
				fmt.Sprintf(
					`CREATE SEQUENCE lane_race_seen; CREATE FUNCTION suppress_lane_candidate() RETURNS `+
						`trigger LANGUAGE plpgsql AS $$BEGIN PERFORM nextval('lane_race_seen'); RETURN NULL; `+
						`END$$; CREATE TRIGGER suppress_lane_candidate BEFORE UPDATE ON fry_member_file_lane `+
						`FOR EACH ROW WHEN (NEW.member='%s' AND NEW.reserved_records>OLD.reserved_records) `+
						`EXECUTE FUNCTION suppress_lane_candidate()`,
					selected.id.String(),
				),
			)
			requireReceiptNoError(t, err)
			attempt := newID(t)
			{
				_, err = capacityAccept(ctx, f, attempt, f.envelope(t), "never dispatched")
				requireReceiptCondition(
					t,
					errors.Is(err, ErrBusy),
					"lost candidate did not exhaust bounded private retries",
					err,
				)
			}
			var hits int64
			{
				err = f.pool.QueryRow(ctx, `SELECT last_value FROM lane_race_seen`).Scan(&hits)
				requireReceiptCondition(t, err == nil, "retry count", hits, err)
				requireReceiptCondition(t, hits == 8, "retry count", hits, err)
			}
			requireLostCapacityCandidateRollback(t, f, attempt)
		})
	}
}

type capacityRollbackReplyError struct {
	pgx.Tx
	calls int
}

func (x *capacityRollbackReplyError) Rollback(ctx context.Context) error {
	x.calls++
	err := x.Tx.Rollback(ctx)
	if err != nil {
		return err
	}
	return errors.New("synthetic lost rollback reply")
}

func TestMemberCapacityCandidateRollbackUncertaintyIsNotRetried(t *testing.T) {
	f := setupWithPoolConfig(
		t,
		nil,
		twoCapacityLaneOptions(2, 0),
		Options{Lease: 10 * time.Second, RecoveryRetention: time.Hour},
	)
	ctx := context.Background()
	_, err := f.pool.Exec(
		ctx,
		`CREATE FUNCTION suppress_lane_candidate() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN `+
			`RETURN NULL; END$$; CREATE TRIGGER suppress_lane_candidate BEFORE UPDATE ON `+
			`fry_member_file_lane FOR EACH ROW WHEN (NEW.reserved_records>OLD.reserved_records) `+
			`EXECUTE FUNCTION suppress_lane_candidate()`,
	)
	requireReceiptNoError(t, err)
	tx, err := f.s.db.begin(ctx)
	requireReceiptNoError(t, err)
	wrapped := &capacityRollbackReplyError{Tx: tx}
	attempt := newID(t)
	env, data, err := normalize(f.envelope(t))
	requireReceiptNoError(t, err)
	_, _, err = f.s.db.prepareMembersTx(
		ctx,
		wrapped,
		attempt,
		env,
		data,
		[2]MemberIdentity{f.members[0].identity(), f.members[1].identity()},
		0,
	)
	requireReceiptCondition(t, errors.Is(err, ErrUncertain), "unconfirmed rollback became retriable", err)
	requireReceiptCondition(t, !errors.Is(err, errCapacityRace), "unconfirmed rollback became retriable", err)
	requireReceiptCondition(t, wrapped.calls >= 1, "rollback was not attempted")
	for _, m := range f.members {
		n, b, _ := capacityCounters(t, f, m)
		requireReceiptCondition(t, n == 0, "actual rollback left debit", n, b)
		requireReceiptCondition(t, b == 64, "actual rollback left debit", n, b)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_attempt WHERE id=$1`, attempt.String()) == 0,
		"rollback left attempt",
	)
}

func TestMemberCapacityConcurrentCandidateFillRetriesWholePreparation(t *testing.T) {
	f := setupWithPoolConfig(
		t,
		nil,
		twoCapacityLaneOptions(2, 0),
		Options{Lease: 15 * time.Second, RecoveryRetention: time.Hour},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	members := append([]*LocalMember(nil), f.members[:]...)
	sort.Slice(members, func(i, j int) bool { return members[i].id.String() < members[j].id.String() })
	// Select the second sorted member so the first member has already been debited
	// when PostgreSQL rechecks the now-full candidate after its lock wait.
	m := members[1]
	attempt := newID(t)
	lane := capacityLaneStart(attempt, m.id, m.bytes.FileID(), 0, 2)
	locker, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer rollback(context.Background(), locker)
	_, err = locker.Exec(
		ctx,
		`UPDATE fry_member_file_lane SET reserved_bytes=reserved_bytes WHERE member=$1 AND `+
			`file_id=$2 AND lane=$3`,
		m.id.String(),
		m.bytes.FileID().String(),
		lane,
	)
	requireReceiptNoError(t, err)
	done := make(chan admissionResult, 1)
	payload := "original after concurrent capacity loss"
	go func() { r, e := capacityAccept(ctx, f, attempt, f.envelope(t), payload); done <- admissionResult{r, e} }()
	fillLockedCapacityCandidate(t, ctx, cancel, f, locker, m, lane, done)
	var got admissionResult
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("retry did not finish")
	}
	requireReceiptCondition(t, got.err == nil, got.err)
	requireReceiptCondition(t, lane >= 0, "invalid capacity lane")
	requireReceiptCondition(t, lane < 16, "invalid capacity lane")
	requireReceiptCondition(
		t,
		int64(capacityAssignedLane(t, f, claim{Attempt: attempt}, m)) != lane,
		"retry reused exhausted candidate",
	)
	requireCapacityCandidateRetryCounters(t, f, members, m, attempt, payload)
	requireReceiptCondition(t, got.receipt.Attempt == attempt, "retry changed attempt identity")
}

func TestMemberCapacityRewritePreservesSurvivorLaneAndReleasesDeadLane(t *testing.T) {
	f := setupWithPoolConfig(
		t,
		nil,
		twoCapacityLaneOptions(2, 0),
		Options{Lease: 10 * time.Second, RecoveryRetention: time.Second},
	)
	live := capacityDeadAndLive(t, f)
	ctx := context.Background()
	var lanes [2]int16
	var old [2]ID
	for i, m := range f.members {
		lanes[i] = capacityAssignedLane(t, f, claim{Attempt: live.Attempt}, m)
		old[i] = m.bytes.FileID()
	}
	for i, m := range f.members {
		requireReceiptNoError(t, f.s.ReclaimHot(ctx, m.id))
		requireReceiptCondition(t, m.bytes.FileID() != old[i], "rewrite changed survivor lane or kept old generation")
		requireReceiptCondition(
			t,
			capacityAssignedLane(t, f, claim{Attempt: live.Attempt}, m) == lanes[i],
			"rewrite changed survivor lane or kept old generation",
		)
		capacityReadTarget(t, f, claim{Attempt: live.Attempt}, m, []byte("live bounded original"))
		actual, receiptErr4 := hotstore.EncodedRecordBytes(live.Size)
		requireReceiptNoError(t, receiptErr4)
		n, b, _ := capacityCounters(t, f, m)
		requireReceiptCondition(t, n == 1, "rewrite retained dead reservation", n, b)
		requireReceiptCondition(t, b == 64+actual, "rewrite retained dead reservation", n, b)
		requireReceiptCondition(
			t,
			scalar(
				t,
				f.pool,
				`SELECT COALESCE(sum(reserved_records+reserved_bytes),0)::bigint FROM `+
					`fry_member_file_lane WHERE member=$1 AND file_id=$2`,
				m.id.String(),
				old[i].String(),
			) == 0,
			"removed file retained charged lane debt",
		)
	}
}

func TestMemberCapacityConservativeTwoDimensionalFragmentation(t *testing.T) {
	opts := twoCapacityLaneOptions(18, 0)
	f := setupWithPoolConfig(t, nil, opts, Options{Lease: 10 * time.Second, RecoveryRetention: time.Hour})
	ctx := context.Background()
	// Search only pure deterministic IDs before admission; every occupied slot
	// below comes from an actual two-copy Store.Accept, never fabricated authority.
	chosen := capacityLaneAttemptSelector(t, f)
	for i := range 9 {
		{
			_, err := capacityAccept(ctx, f, chosen(0), f.envelope(t), "x")
			requireReceiptCondition(t, err == nil, "tiny record fill", i, err)
		}
	}
	full := string(bytes.Repeat([]byte("L"), int(opts.MaxMessageBytes)))
	for i := range 8 {
		{
			_, err := capacityAccept(ctx, f, chosen(1), f.envelope(t), full)
			requireReceiptCondition(t, err == nil, "byte fill", i, err)
		}
	}
	encoded, receiptErr5 := hotstore.EncodedRecordBytes(opts.MaxMessageBytes)
	requireReceiptNoError(t, receiptErr5)
	var before [2][3]int64
	for i, m := range f.members {
		before[i][0], before[i][1], before[i][2] = capacityCounters(t, f, m)
		requireReceiptCondition(
			t,
			before[i][0] < opts.MaxRecords,
			"fixture lacks aggregate spare slot/bytes",
			before[i],
		)
		requireReceiptCondition(
			t,
			opts.MaxFileBytes-before[i][1] >= encoded,
			"fixture lacks aggregate spare slot/bytes",
			before[i],
		)
		requireIndependentCapacityLanePressure(t, ctx, f, m, encoded)
	}
	attempt := newID(t)
	{
		_, err := capacityAccept(ctx, f, attempt, f.envelope(t), "conservative rejection")
		requireReceiptCondition(t, errors.Is(err, hotstore.ErrCapacity), "fragmented aggregate was overbooked", err)
	}
	for i, m := range f.members {
		n, b, c := capacityCounters(t, f, m)
		requireReceiptCondition(t, [3]int64{n, b, c} == before[i], "fragmentation rejection leaked reservation")
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_attempt WHERE id=$1`, attempt.String()) == 0,
		"fragmentation rejection left preparation",
	)
}

func waitSameLaneRefundBlocked(
	t *testing.T,
	ctx context.Context,
	cancel context.CancelFunc,
	f *fixture,
	locker pgx.Tx,
	doneA <-chan error,
) {
	t.Helper()
	blockedCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	err := waitCapacityBlock(blockedCtx, f, locker.Conn().PgConn().PID())
	stop()
	if err != nil {
		cancel()
		rollback(context.WithoutCancel(ctx), locker)
		if drainErr := <-doneA; drainErr != nil {
			t.Error(drainErr)
		}
		t.Fatal("same-lane real refund did not wait on its budget writer", err)
	}
}

func awaitDifferentLaneRefund(
	t *testing.T,
	cancel context.CancelFunc,
	locker pgx.Tx,
	doneA, doneB <-chan error,
) (context.Context, context.CancelFunc) {
	t.Helper()
	var errB error
	finishedB := false
	select {
	case errB = <-doneB:
		finishedB = true
	case <-time.After(3 * time.Second):
	}
	releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if !finishedB {
		failBlockedDifferentLaneRefund(t, cancel, locker, releaseCtx, releaseCancel, doneA, doneB)
	}
	if errB != nil {
		reportCapacityLaneRollback(t, releaseCtx, locker)
		releaseCancel()
		cancel()
		if drainErr := <-doneA; drainErr != nil {
			t.Error(drainErr)
		}
		t.Fatal(errB)
	}
	select {
	case early := <-doneA:
		reportCapacityLaneRollback(t, releaseCtx, locker)
		releaseCancel()
		t.Fatal("same-lane writer bypassed lock", early)
	default:
	}
	return releaseCtx, releaseCancel
}

func failBlockedDifferentLaneRefund(
	t *testing.T,
	cancel context.CancelFunc,
	locker pgx.Tx,
	releaseCtx context.Context,
	releaseCancel context.CancelFunc,
	doneA, doneB <-chan error,
) {
	t.Helper()
	releaseErr := locker.Rollback(releaseCtx)
	releaseCancel()
	cancel()
	select {
	case drainErr := <-doneA:
		if drainErr != nil {
			t.Error(drainErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked writer failed to join")
	}
	select {
	case drainErr := <-doneB:
		if drainErr != nil {
			t.Error(drainErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("independent writer failed to join")
	}
	t.Fatal("different lane refund waited behind another lane", releaseErr)
}

func requireSameLaneTargetUnsettled(
	t *testing.T,
	ctx context.Context,
	cancel context.CancelFunc,
	f *fixture,
	first claim,
	m *LocalMember,
	locker pgx.Tx,
	releaseCtx context.Context,
	releaseCancel context.CancelFunc,
	doneA <-chan error,
) {
	t.Helper()
	var err error
	var firstSettled bool
	if err = f.pool.QueryRow(
		ctx,
		`SELECT capacity_settled FROM fry_target WHERE attempt=$1 AND member=$2`,
		first.Attempt.String(),
		m.id.String(),
	).
		Scan(&firstSettled); err != nil ||
		firstSettled {
		reportCapacityLaneRollback(t, releaseCtx, locker)
		releaseCancel()
		cancel()
		if drainErr := <-doneA; drainErr != nil {
			t.Error(drainErr)
		}
		t.Fatal("blocked target published early", err)
	}
}

func releaseSameLaneRefund(
	t *testing.T,
	cancel context.CancelFunc,
	locker pgx.Tx,
	releaseCtx context.Context,
	releaseCancel context.CancelFunc,
	doneA <-chan error,
) {
	t.Helper()
	var err error
	if err = locker.Rollback(releaseCtx); err != nil {
		releaseCancel()
		cancel()
		if drainErr := <-doneA; drainErr != nil {
			t.Error(drainErr)
		}
		t.Fatal(err)
	}
	releaseCancel()
	select {
	case err = <-doneA:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("same-lane writer failed to finish after release")
	}
	requireReceiptNoError(t, err)
}

func requireCapacityLaneRefundCounters(
	t *testing.T,
	f *fixture,
	m *LocalMember,
	first, second claim,
	laneA, laneB int16,
	payloadA, payloadB []byte,
) {
	t.Helper()
	requireReceiptCondition(
		t,
		capacityAssignedLane(t, f, first, m) == laneA,
		"settlement silently switched reservation lane",
	)
	requireReceiptCondition(
		t,
		capacityAssignedLane(t, f, second, m) == laneB,
		"settlement silently switched reservation lane",
	)
	expectedA, receiptErr1 := hotstore.EncodedRecordBytes(int64(len(payloadA)))
	requireReceiptNoError(t, receiptErr1)
	expectedB, receiptErr2 := hotstore.EncodedRecordBytes(int64(len(payloadB)))
	requireReceiptNoError(t, receiptErr2)
	n, b, _ := capacityCounters(t, f, m)
	requireReceiptCondition(t, n == 2, "independent refunds changed aggregate capacity", n, b)
	requireReceiptCondition(t, b == 64+expectedA+expectedB, "independent refunds changed aggregate capacity", n, b)
}

func requireCapacityCandidateRetryCounters(
	t *testing.T,
	f *fixture,
	members []*LocalMember,
	m *LocalMember,
	attempt ID,
	payload string,
) {
	t.Helper()
	actual, receiptErr3 := hotstore.EncodedRecordBytes(int64(len(payload)))
	requireReceiptNoError(t, receiptErr3)
	for _, member := range members {
		n, b, _ := capacityCounters(t, f, member)
		wantN, wantB := int64(1), 64+actual
		if member == m {
			wantN++
			wantB += (m.opts.MaxFileBytes - 64) / 2
		}
		requireReceiptCondition(t, n == wantN, "candidate retry leaked partial debit", member.id, n, b, wantN, wantB)
		requireReceiptCondition(t, b == wantB, "candidate retry leaked partial debit", member.id, n, b, wantN, wantB)
		capacityReadTarget(t, f, claim{Attempt: attempt}, member, []byte(payload))
	}
}

func fillLockedCapacityCandidate(
	t *testing.T,
	ctx context.Context,
	cancel context.CancelFunc,
	f *fixture,
	locker pgx.Tx,
	m *LocalMember,
	lane int64,
	done <-chan admissionResult,
) {
	t.Helper()
	waitCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	err := waitCapacityBlock(waitCtx, f, locker.Conn().PgConn().PID())
	stop()
	if err != nil {
		cancel()
		rollback(context.WithoutCancel(ctx), locker)
		if drainErr := <-done; drainErr.err != nil {
			t.Error(drainErr.err)
		}
		t.Fatal("preparation did not reach actual candidate lock wait", err)
	}
	// This is a reservation-ledger contention fixture, not a fabricated receipt.
	// The unrelated occupancy remains charged; only the new preparation may retry.
	if _, err = locker.Exec(
		ctx,
		`UPDATE fry_member_file_lane SET `+
			`reserved_records=record_ceiling,reserved_bytes=byte_ceiling WHERE member=$1 AND `+
			`file_id=$2 AND lane=$3`,
		m.id.String(),
		m.bytes.FileID().String(),
		lane,
	); err != nil {
		cancel()
		rollback(context.WithoutCancel(ctx), locker)
		if drainErr := <-done; drainErr.err != nil {
			t.Error(drainErr.err)
		}
		t.Fatal(err)
	}
	if err = locker.Commit(ctx); err != nil {
		cancel()
		if drainErr := <-done; drainErr.err != nil {
			t.Error(drainErr.err)
		}
		t.Fatal(err)
	}
}

func capacityLaneAttemptSelector(t *testing.T, f *fixture) func(int64) ID {
	t.Helper()
	next := uint64(1)
	chosen := func(want int64) ID {
		for range 4096 {
			var id ID
			binary.BigEndian.PutUint64(id[8:], next)
			next++
			matched := true
			for _, m := range f.members {
				if capacityLaneStart(id, m.id, m.bytes.FileID(), 0, 2) != want {
					matched = false
					break
				}
			}
			if matched {
				return id
			}
		}
		t.Fatal("no deterministic fixture ID for lane")
		return ID{}
	}
	return chosen
}

func reportCapacityLaneRollback(t *testing.T, ctx context.Context, locker pgx.Tx) {
	t.Helper()
	if err := locker.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Error(err)
	}
}

func requireLostCapacityCandidateRollback(t *testing.T, f *fixture, attempt ID) {
	t.Helper()
	for _, m := range f.members {
		n, b, _ := capacityCounters(t, f, m)
		requireReceiptCondition(t, n == 0, "lost candidate leaked earlier member debit or bytes", n, b)
		requireReceiptCondition(t, b == 64, "lost candidate leaked earlier member debit or bytes", n, b)
		requireReceiptCondition(
			t,
			len(m.bytes.Inventory()) == 0,
			"lost candidate leaked earlier member debit or bytes",
			n,
			b,
		)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_attempt WHERE id=$1`, attempt.String()) == 0,
		"lost candidate retained half preparation",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_target WHERE attempt=$1`, attempt.String()) == 0,
		"lost candidate retained half preparation",
	)
}

func requireIndependentCapacityLanePressure(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	m *LocalMember,
	encoded int64,
) {
	t.Helper()
	var recordFull, byteFull bool
	{
		err := f.pool.QueryRow(ctx, `SELECT bool_or(reserved_records=record_ceiling AND `+
			`byte_ceiling-reserved_bytes>=$3),bool_or(reserved_records<record_ceiling AND `+
			`byte_ceiling-reserved_bytes<$3) FROM fry_member_file_lane WHERE member=$1 AND `+
			`file_id=$2`, m.id.String(), m.bytes.FileID().String(), encoded).
			Scan(&recordFull, &byteFull)
		requireReceiptCondition(
			t,
			err == nil,
			"fixture lacks independent record/byte pressure",
			recordFull,
			byteFull,
			err,
		)
		requireReceiptCondition(
			t,
			recordFull,
			"fixture lacks independent record/byte pressure",
			recordFull,
			byteFull,
			err,
		)
		requireReceiptCondition(
			t,
			byteFull,
			"fixture lacks independent record/byte pressure",
			recordFull,
			byteFull,
			err,
		)
	}
}
