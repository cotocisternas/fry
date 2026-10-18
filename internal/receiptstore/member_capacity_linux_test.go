//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"

	"github.com/jackc/pgx/v5"
)

func capacityOptions(records int64) hotstore.Options {
	e := encodedTestBytes(1024)
	c := int64(64) + records*e
	return hotstore.Options{
		MaxMessageBytes:      1024,
		MaxConcurrentAppends: 2,
		MaxRecords:           records,
		MaxFileBytes:         c,
		MaxBytes:             2*c + 4*e,
	}
}

func capacityFixture(t *testing.T, records int64) *fixture {
	t.Helper()
	return setupWithPoolConfig(
		t,
		nil,
		capacityOptions(records),
		Options{Lease: 10 * time.Second, RecoveryRetention: time.Second},
	)
}

func capacityCounters(t *testing.T, f *fixture, m *LocalMember) (int64, int64, int64) {
	t.Helper()
	return capacityCountersContext(context.Background(), t, f, m)
}

func capacityAccept(ctx context.Context, f *fixture, id ID, e Envelope, body string) (Receipt, error) {
	return f.s.Accept(ctx, id, e, io.NopCloser(bytes.NewBufferString(body)))
}

func capacityDeadAndLive(t *testing.T, f *fixture) Receipt {
	t.Helper()
	ctx := context.Background()
	e := f.envelope(t)
	dead, err := capacityAccept(ctx, f, newID(t), e, "dead bounded original")
	requireReceiptNoError(t, err)
	live, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "live bounded original")
	requireReceiptNoError(t, err)
	err = f.s.ReleaseReference(
		ctx,
		f.org,
		dead.ID,
		Reference{Kind: "pending-mailbox", Key: e.Destinations[0].Inbox.String()},
	)
	requireReceiptNoError(t, err)
	time.Sleep(1100 * time.Millisecond)
	_, err = f.s.SweepExpiry(ctx, 10)
	requireReceiptNoError(t, err)
	return live
}

func TestMemberCapacitySettlementRetryAndCapAreExact(t *testing.T) {
	f := capacityFixture(t, 2)
	ctx := context.Background()
	e := f.envelope(t)
	attempt := newID(t)
	claim := prepare(t, f, attempt, e)
	maxEncoded, receiptErr1 := hotstore.EncodedRecordBytes(1024)
	requireReceiptNoError(t, receiptErr1)
	for _, m := range f.members {
		n, b, c := capacityCounters(t, f, m)
		requireReceiptCondition(t, n == 1, "unsettled reservation wrong", n, b, c)
		requireReceiptCondition(t, b == 64+maxEncoded, "unsettled reservation wrong", n, b, c)
		requireReceiptCondition(t, c == m.opts.MaxFileBytes, "unsettled reservation wrong", n, b, c)
	}
	copies(t, f, claim, "short exact body")
	actual, receiptErr2 := hotstore.EncodedRecordBytes(int64(len("short exact body")))
	requireReceiptNoError(t, receiptErr2)
	for _, m := range f.members {
		n, b, _ := capacityCounters(t, f, m)
		requireReceiptCondition(t, n == 1, "actual settlement wrong", n, b)
		requireReceiptCondition(t, b == 64+actual, "actual settlement wrong", n, b)
		requireReceiptCondition(
			t,
			scalar(
				t,
				f.pool,
				`SELECT count(*) FROM fry_target WHERE attempt=$1 AND member=$2 AND capacity_settled `+
					`AND capacity_reserved_bytes=$3`,
				attempt.String(),
				m.id.String(),
				actual,
			) == 1,
			"target settlement missing",
		)
	}
	// Simulate a caller resuming after durable-copy replies and lease expiry.
	expire(t, f, attempt)
	receipt, err := capacityAccept(ctx, f, attempt, e, "short exact body")
	requireReceiptNoError(t, err)
	replay, err := capacityAccept(ctx, f, attempt, e, "ignored accepted replay")
	requireReceiptCondition(t, err == nil, "accepted replay changed", err)
	requireReceiptCondition(t, replay == receipt, "accepted replay changed", err)
	_, err = capacityAccept(ctx, f, newID(t), f.envelope(t), "second")
	requireReceiptNoError(t, err)
	blocked := newID(t)
	{
		_, err = capacityAccept(ctx, f, blocked, f.envelope(t), "third")
		requireReceiptCondition(t, errors.Is(err, hotstore.ErrCapacity), "record cap not enforced", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_attempt WHERE id=$1`, blocked.String()) == 0,
		"capacity rejection left attempt",
	)
	for _, m := range f.members {
		n, _, _ := capacityCounters(t, f, m)
		requireReceiptCondition(t, n == 2, "retry recharged", n)
	}
}

func TestMemberCapacitySecondTargetFailureRollsBackFirst(t *testing.T) {
	f := capacityFixture(t, 2)
	ctx := context.Background()
	capacityDeadAndLive(t, f)
	sorted := append([]*LocalMember(nil), f.members[:]...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].id.String() < sorted[j].id.String() })
	requireReceiptNoError(t, f.s.ReclaimHot(ctx, sorted[0].id))
	n, b, c := capacityCounters(t, f, sorted[0])
	requireReceiptCondition(t, n == 1, "census did not free first member slot")
	other, _, _ := capacityCounters(t, f, sorted[1])
	requireReceiptCondition(t, other == 2, "test requires second member full")
	attempt := newID(t)
	{
		_, err := capacityAccept(ctx, f, attempt, f.envelope(t), "must roll back first member reservation")
		requireReceiptCondition(t, errors.Is(err, hotstore.ErrCapacity), "second member did not reject", err)
	}
	nn, bb, cc := capacityCounters(t, f, sorted[0])
	requireReceiptCondition(t, n == nn, "first member reservation leaked", n, nn, b, bb)
	requireReceiptCondition(t, b == bb, "first member reservation leaked", n, nn, b, bb)
	requireReceiptCondition(t, c == cc, "first member reservation leaked", n, nn, b, bb)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_attempt WHERE id=$1`, attempt.String()) == 0,
		"rolled back preparation left authority",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_target WHERE attempt=$1`, attempt.String()) == 0,
		"rolled back preparation left authority",
	)
}

func TestMemberCapacityAbortRetainsUntilCompleteCensus(t *testing.T) {
	f := capacityFixture(t, 1)
	ctx := context.Background()
	attempt := newID(t)
	claim := prepare(t, f, attempt, f.envelope(t))
	copies(t, f, claim, "unaccepted durable bytes")
	requireReceiptNoError(t, f.s.Abort(ctx, f.org, attempt))
	{
		_, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "too early")
		requireReceiptCondition(t, errors.Is(err, hotstore.ErrCapacity), "abort returned occupied capacity", err)
	}
	for _, m := range f.members {
		n, _, _ := capacityCounters(t, f, m)
		requireReceiptCondition(t, n == 1, "abort prematurely forgot occupancy")
		requireReceiptNoError(t, f.s.ReclaimHot(ctx, m.id))
		n, b, _ := capacityCounters(t, f, m)
		requireReceiptCondition(t, n == 0, "complete census did not free dead occupancy", n, b)
		requireReceiptCondition(t, b == 64, "complete census did not free dead occupancy", n, b)
	}
	{
		_, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "now admitted")
		requireReceiptCondition(t, err == nil, err)
	}
}

func TestMemberCapacityFrozenProfileRefusesBeforeFilesystemMutation(t *testing.T) {
	for _, mode := range []string{"bounded-numeric", "bounded-to-legacy", "legacy-to-bounded"} {
		t.Run(mode, func(t *testing.T) {
			testFrozenCapacityProfile(t, mode)
		})
	}
}

func TestMemberCapacityRewriteKeepsPredecessorChargeUntilUnlink(t *testing.T) {
	f := capacityFixture(t, 2)
	ctx := context.Background()
	capacityDeadAndLive(t, f)
	m := f.members[0]
	old := m.bytes.FileID()
	c := m.opts.MaxFileBytes
	oldInfo, err := os.Stat(filepath.Join(m.root, "data.fry"))
	requireReceiptNoError(t, err)
	m.rewriteFault = func(at string) error {
		if at == "after_publish" {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	{
		err := f.s.ReclaimHot(ctx, m.id)
		requireReceiptCondition(t, err != nil, "publication fault missing")
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT sum(charged_bytes)::bigint FROM fry_member_file WHERE member=$1 AND `+
				`state<>'removed'`,
			m.id.String(),
		) == c+oldInfo.Size(),
		"predecessor capacity released before unlink",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT reserved_bytes FROM fry_hot_rewrite WHERE member=$1`, m.id.String()) == 0,
		"successor capacity charged in both job and catalog",
	)
	{
		_, err := os.Stat(filepath.Join(m.root, "data.fry"))
		requireReceiptCondition(t, err == nil, "old bytes gone before test checkpoint", err)
	}
	requireReceiptNoError(t, m.Close())
	next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, capacityOptions(2))
	requireReceiptNoError(t, err)
	defer func() {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
	}()
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT charged_bytes FROM fry_member_file WHERE member=$1 AND file_id=$2`,
			m.id.String(),
			old.String(),
		) == 0,
		"unlinked predecessor remains charged",
	)
	requireCapacityRewriteSurvivorCensus(t, f, next, c)
}

func TestMemberCapacityConcurrentAdmissionCannotOverbook(t *testing.T) {
	f := capacityFixture(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "concurrent original")
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success, rejected := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, hotstore.ErrCapacity) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	requireReceiptCondition(t, success == 1, "concurrent cap", success, rejected)
	requireReceiptCondition(t, rejected == 1, "concurrent cap", success, rejected)
	for _, m := range f.members {
		n, _, _ := capacityCounters(t, f, m)
		requireReceiptCondition(t, n == 1, "concurrent quota overbooked actual records", n)
		requireReceiptCondition(t, len(m.bytes.Inventory()) == 1, "concurrent quota overbooked actual records", n)
	}
}

func TestMemberCapacityClosedAbortWithoutRecordedCopyFreesOnlyAfterCensus(t *testing.T) {
	for _, physical := range []bool{false, true} {
		t.Run(fmt.Sprintf("physical-%t", physical), func(t *testing.T) {
			f := capacityFixture(t, 1)
			ctx := context.Background()
			attempt := newID(t)
			claim := prepare(t, f, attempt, f.envelope(t))
			maxEncoded, receiptErr4 := hotstore.EncodedRecordBytes(1024)
			requireReceiptNoError(t, receiptErr4)
			if physical {
				for _, m := range f.members {
					{
						_, err := m.bytes.Append(
							ctx,
							hotstore.Key{Receipt: claim.Receipt, Attempt: claim.Attempt},
							bytes.NewBufferString("lost recordCopy reply"),
						)
						requireReceiptCondition(t, err == nil, err)
					}
				}
			}
			requireReceiptNoError(t, f.s.Abort(ctx, f.org, attempt))
			for _, m := range f.members {
				n, b, _ := capacityCounters(t, f, m)
				requireReceiptCondition(t, n == 1, "abort or lost publication prematurely credited capacity", n, b)
				requireReceiptCondition(
					t,
					b == 64+maxEncoded,
					"abort or lost publication prematurely credited capacity",
					n,
					b,
				)
			}
			{
				_, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "not before census")
				requireReceiptCondition(
					t,
					errors.Is(err, hotstore.ErrCapacity),
					"unsettled closed target forgot reservation",
					err,
				)
			}
			for _, m := range f.members {
				requireReceiptNoError(t, f.s.ReclaimHot(ctx, m.id))
				n, b, _ := capacityCounters(t, f, m)
				requireReceiptCondition(t, n == 0, "closed target census did not free unused reservation", n, b)
				requireReceiptCondition(t, b == 64, "closed target census did not free unused reservation", n, b)
				requireReceiptCondition(
					t,
					len(m.bytes.Inventory()) == 0,
					"closed target census did not free unused reservation",
					n,
					b,
				)
			}
			{
				_, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "after actual census")
				requireReceiptCondition(t, err == nil, err)
			}
		})
	}
}

func TestMemberCapacityNearCapRewritePreservesPhysicalAppendOrder(t *testing.T) {
	ctx := context.Background()
	opts := capacityOptions(3)
	maxEncoded, receiptErr5 := hotstore.EncodedRecordBytes(1024)
	requireReceiptNoError(t, receiptErr5)
	deadEncoded, receiptErr6 := hotstore.EncodedRecordBytes(1)
	requireReceiptNoError(t, receiptErr6)
	smallEncoded, receiptErr7 := hotstore.EncodedRecordBytes(32)
	requireReceiptNoError(t, receiptErr7)
	opts.MaxFileBytes = 64 + deadEncoded + smallEncoded + maxEncoded
	opts.MaxBytes = 2*opts.MaxFileBytes + 4*maxEncoded
	f := setupWithPoolConfig(t, nil, opts, Options{Lease: 10 * time.Second, RecoveryRetention: time.Second})
	deadAttempt, smallAttempt, largeAttempt := newID(t), newID(t), newID(t)
	// Control only receipt-ID allocation in this fixture, so SQL receipt order
	// reverses the two keepers. All bytes and accepted authority still use Accept.
	_, err := f.pool.Exec(
		ctx,
		fmt.Sprintf(
			`CREATE FUNCTION deterministic_capacity_receipt() RETURNS trigger LANGUAGE plpgsql AS `+
				`$$BEGIN IF NEW.id='%s' THEN NEW.receipt_id='00000000000000000000000000000003'; ELSIF `+
				`NEW.id='%s' THEN NEW.receipt_id='00000000000000000000000000000002'; ELSIF NEW.id='%s' `+
				`THEN NEW.receipt_id='00000000000000000000000000000001'; END IF; RETURN NEW; END$$; `+
				`CREATE TRIGGER deterministic_capacity_receipt BEFORE INSERT ON fry_attempt FOR EACH `+
				`ROW EXECUTE FUNCTION deterministic_capacity_receipt()`,
			deadAttempt.String(),
			smallAttempt.String(),
			largeAttempt.String(),
		),
	)
	requireReceiptNoError(t, err)
	e := f.envelope(t)
	dead, err := capacityAccept(ctx, f, deadAttempt, e, "d")
	requireReceiptNoError(t, err)
	smallBody, largeBody := string(bytes.Repeat([]byte("s"), 32)), string(bytes.Repeat([]byte("L"), 800))
	small, err := capacityAccept(ctx, f, smallAttempt, f.envelope(t), smallBody)
	requireReceiptNoError(t, err)
	large, err := capacityAccept(ctx, f, largeAttempt, f.envelope(t), largeBody)
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, small.ID.String() >= large.ID.String(), "fixture did not reverse receipt order")
	err = f.s.ReleaseReference(
		ctx,
		f.org,
		dead.ID,
		Reference{Kind: "pending-mailbox", Key: e.Destinations[0].Inbox.String()},
	)
	requireReceiptNoError(t, err)
	time.Sleep(1100 * time.Millisecond)
	_, err = f.s.SweepExpiry(ctx, 10)
	requireReceiptNoError(t, err)
	requireCapacityRewriteAppendOrder(t, f, small, large, smallBody, largeBody)
}

func TestMemberCapacityCorruptCurrentLedgerRefusesBeforeFilesystemMutation(t *testing.T) {
	for _, field := range []string{"zero-charge", "records-over-cap", "bytes-over-cap"} {
		t.Run(field, func(t *testing.T) {
			f := capacityFixture(t, 2)
			ctx := context.Background()
			m := f.members[0]
			requireReceiptNoError(t, m.Close())
			query := `UPDATE fry_member_file SET charged_bytes=0 WHERE member=$1`
			switch field {
			case "records-over-cap":
				query = `UPDATE fry_member_file_lane SET record_ceiling=3,reserved_records=3 WHERE member=$1`
			case "bytes-over-cap":
				query = `UPDATE fry_member_file_lane SET byte_ceiling=$2,reserved_bytes=$2 WHERE member=$1`
			}
			var err error
			if field == "bytes-over-cap" {
				_, err = f.pool.Exec(ctx, query, m.id.String(), m.opts.MaxFileBytes+1)
			} else {
				_, err = f.pool.Exec(ctx, query, m.id.String())
			}
			requireReceiptNoError(t, err)
			err = os.WriteFile(filepath.Join(m.root, "stage-ledger-sentinel"), []byte("must remain"), 0o600)
			requireReceiptNoError(t, err)
			before := memberFilesTree(t, m.root)
			reopened, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, capacityOptions(2))
			if err == nil {
				if err := reopened.Close(); err != nil {
					t.Error(err)
				}
				t.Fatal("corrupt bounded ledger opened", field)
			}
			memberFilesUnchanged(t, m.root, before)
		})
	}
}

func TestMemberCapacityWrongSuccessorReservationRefusesBeforeFilesystemMutation(t *testing.T) {
	f := capacityFixture(t, 2)
	ctx := context.Background()
	capacityDeadAndLive(t, f)
	m := f.members[0]
	m.rewriteFault = func(at string) error {
		if at == "before_register" {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	{
		err := f.s.ReclaimHot(ctx, m.id)
		requireReceiptCondition(t, err != nil, "rewrite cut missing")
	}
	requireReceiptNoError(t, m.Close())
	var dir string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(
			ctx,
			`SELECT new_directory FROM fry_hot_rewrite WHERE member=$1 AND state='building'`,
			m.id.String(),
		).
			Scan(&dir),
	)
	// Model the earlier crash point with a reserved job but no successor files.
	requireReceiptNoError(t, os.RemoveAll(filepath.Join(m.root, dir)))
	{
		_, err := f.pool.Exec(
			ctx,
			`UPDATE fry_hot_rewrite SET reserved_bytes=$2 WHERE member=$1 AND state='building'`,
			m.id.String(),
			m.opts.MaxFileBytes-1,
		)
		requireReceiptCondition(t, err == nil, err)
	}
	before := memberFilesTree(t, m.root)
	next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, capacityOptions(2))
	if err == nil {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("wrong durable successor capacity admitted")
	}
	memberFilesUnchanged(t, m.root, before)
}

func TestMemberCapacityOverbudgetActiveLedgerRefusesBeforeFilesystemMutation(t *testing.T) {
	f := capacityFixture(t, 2)
	ctx := context.Background()
	capacityDeadAndLive(t, f)
	m := f.members[0]
	m.rewriteFault = func(at string) error {
		if at == "after_publish" {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	{
		err := f.s.ReclaimHot(ctx, m.id)
		requireReceiptCondition(t, err != nil, "publication cut missing")
	}
	// Individually shaped current/retiring entries plus an additional preregistered
	// successor job exceed the member budget. No bytes are fabricated or removed.
	_, err := f.pool.Exec(
		ctx,
		`INSERT INTO `+
			`fry_hot_rewrite(id,member,old_incarnation,old_directory,new_directory,state,`+
			`reserved_bytes) SELECT $1,id,incarnation,directory,$3,'building',$4 `+
			`FROM fry_member WHERE id=$2`,
		newID(t).String(),
		m.id.String(),
		"generation-"+newID(t).String(),
		m.opts.MaxFileBytes,
	)
	requireReceiptNoError(t, err)
	total := scalar(
		t,
		f.pool,
		`SELECT (COALESCE((SELECT sum(charged_bytes) FROM fry_member_file WHERE member=$1 AND `+
			`state<>'removed'),0)+COALESCE((SELECT sum(reserved_bytes) FROM fry_hot_rewrite WHERE `+
			`member=$1 AND state='building'),0)+(SELECT staging_bytes FROM fry_member_profile `+
			`WHERE member=$1))::bigint`,
		m.id.String(),
	)
	requireReceiptCondition(t, total > m.opts.MaxBytes, "fixture not over budget")
	err = m.Close()
	requireReceiptNoError(t, err)
	before := memberFilesTree(t, m.root)
	next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, capacityOptions(2))
	if err == nil {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("overbudget active ledger repaired instead of refusing")
	}
	memberFilesUnchanged(t, m.root, before)
}

func TestMemberCapacityCurrentPhysicalBytesCannotExceedReservedLedger(t *testing.T) {
	f := capacityFixture(t, 2)
	ctx := context.Background()
	{
		_, err := capacityAccept(
			ctx,
			f,
			newID(t),
			f.envelope(t),
			"actual accepted original has framed bytes beyond the header",
		)
		requireReceiptCondition(t, err == nil, err)
	}
	m := f.members[0]
	requireReceiptNoError(t, m.Close())
	{
		_, err := f.pool.Exec(ctx, `UPDATE fry_member_file_lane SET reserved_bytes=0 WHERE member=$1`, m.id.String())
		requireReceiptCondition(t, err == nil, err)
	}
	requireReceiptNoError(
		t,
		os.WriteFile(
			filepath.Join(m.root, "stage-undercount-sentinel"),
			[]byte("do not clean on invalid ledger"),
			0o600,
		),
	)
	before := memberFilesTree(t, m.root)
	next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, capacityOptions(2))
	if !errors.Is(err, ErrConflict) {
		if next != nil {
			if err := next.Close(); err != nil {
				t.Error(err)
			}
		}
		t.Fatal("undercharged physical file accepted", err)
	}
	memberFilesUnchanged(t, m.root, before)
}

func TestMemberCapacityFullSizeSettlementDoesNotWaitForFileBudgetWriter(t *testing.T) {
	f := capacityFixture(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	claim := prepare(t, f, newID(t), f.envelope(t))
	m := f.members[0]
	beforeCount, beforeBytes, beforeCharge := capacityCounters(t, f, m)
	locker, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer rollback(context.Background(), locker)
	var heldLane int16
	err = locker.QueryRow(ctx, `UPDATE fry_member_file_lane l SET reserved_bytes=l.reserved_bytes FROM fry_target t `+
		`WHERE t.member=l.member AND t.incarnation=l.file_id AND t.capacity_lane=l.lane AND `+
		`t.attempt=$1 AND t.member=$2 RETURNING l.lane`, claim.Attempt.String(), m.id.String()).
		Scan(&heldLane)
	requireReceiptNoError(t, err)
	lockerPID := locker.Conn().PgConn().PID()
	payload := bytes.Repeat([]byte("F"), int(m.opts.MaxMessageBytes))
	done := make(chan error, 1)
	go func() { done <- m.write(ctx, f.s.db, claim, io.NopCloser(bytes.NewReader(payload))) }()
	blocked, finished, writeErr := observeFullSizeCapacityWrite(t, ctx, cancel, f, locker, lockerPID, done)
	requireFullSizeCapacityWriteUnblocked(t, ctx, cancel, f, m, locker, lockerPID, blocked, finished, writeErr, done)
	requireFullSizeCapacitySettlement(t, ctx, f, m, claim, payload, writeErr, beforeCount, beforeBytes, beforeCharge)
}

func testFrozenCapacityProfile(t *testing.T, mode string) {
	t.Helper()

	var f *fixture
	if mode == "legacy-to-bounded" {
		f = setup(t)
	} else {
		f = capacityFixture(t, 2)
	}
	m := f.members[0]
	requireReceiptNoError(t, m.Close())
	requireReceiptNoError(t, os.WriteFile(filepath.Join(m.root, "stage-profile-sentinel"), []byte("preserve"), 0o600))
	before := memberFilesTree(t, m.root)
	opts := capacityOptions(3)
	if mode == "bounded-to-legacy" {
		opts = hotOptions()
	}
	reopened, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, opts)
	if !errors.Is(err, ErrConflict) {
		if reopened != nil {
			if err := reopened.Close(); err != nil {
				t.Error(err)
			}
		}
		t.Fatal("profile mismatch not refused", mode, err)
	}
	memberFilesUnchanged(t, m.root, before)
}

func requireCapacityRewriteAppendOrder(t *testing.T, f *fixture, small, large Receipt, smallBody, largeBody string) {
	t.Helper()
	ctx := context.Background()
	var err error
	for _, m := range f.members {
		{
			err = f.s.ReclaimHot(ctx, m.id)
			requireReceiptCondition(t, err == nil, "admissible retained prefix failed rewrite", err)
		}
		for _, expected := range []struct {
			receipt Receipt
			body    string
		}{{small, smallBody}, {large, largeBody}} {
			var raw []byte
			err = f.pool.QueryRow(
				ctx,
				`SELECT locator FROM fry_target WHERE attempt=$1 AND member=$2 AND NOT bytes_removed`,
				expected.receipt.Attempt.String(),
				m.id.String(),
			).
				Scan(&raw)
			requireReceiptNoError(t, err)
			var loc hotstore.Locator
			err = json.Unmarshal(raw, &loc)
			requireReceiptNoError(t, err)
			requireReceiptCondition(t, loc.File == m.bytes.FileID(), "new locator does not select rewritten member")
			var out bytes.Buffer
			{
				err = m.bytes.Read(ctx, loc, 0, expected.receipt.Size, &out)
				requireReceiptCondition(t, err == nil, "direct retained original changed", err)
				requireReceiptCondition(t, out.String() == expected.body, "direct retained original changed", err)
			}
		}
	}
}

func observeFullSizeCapacityWrite(
	t *testing.T,
	ctx context.Context,
	cancel context.CancelFunc,
	f *fixture,
	locker pgx.Tx,
	lockerPID uint32,
	done <-chan error,
) (bool, bool, error) {
	t.Helper()
	var err error
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	blocked := false
	finished := false
	var writeErr error
	for !finished && !blocked {
		select {
		case writeErr = <-done:
			finished = true
		case <-deadline.C:
			blocked = true
		case <-ticker.C:
			// Observe actual PostgreSQL lock dependency rather than inferring a slow fsync.
			if err = f.pool.QueryRow(
				ctx,
				`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1::int=ANY(pg_blocking_pids(pid)))`,
				lockerPID,
			).
				Scan(&blocked); err != nil {
				cancel()
				rollback(context.WithoutCancel(ctx), locker)
				if drainErr := <-done; drainErr != nil {
					t.Error(drainErr)
				}
				t.Fatal(err)
			}
		}
	}
	return blocked, finished, writeErr
}

func requireFullSizeCapacityWriteUnblocked(
	t *testing.T,
	ctx context.Context,
	cancel context.CancelFunc,
	f *fixture,
	m *LocalMember,
	locker pgx.Tx,
	lockerPID uint32,
	blocked, finished bool,
	writeErr error,
	done <-chan error,
) {
	t.Helper()
	if blocked {
		var blockedQuery, waitType, waitEvent string
		if e := f.pool.QueryRow(ctx, `SELECT query,COALESCE(wait_event_type,''),COALESCE(wait_event,'') FROM `+
			`pg_stat_activity WHERE $1::int=ANY(pg_blocking_pids(pid)) LIMIT 1`, lockerPID).
			Scan(&blockedQuery, &waitType, &waitEvent); e == nil {
			t.Logf("blocked budget writer: query=%q wait_type=%s wait_event=%s", blockedQuery, waitType, waitEvent)
		}
		durable := len(m.bytes.Inventory()) == 1
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		releaseErr := locker.Rollback(cleanup)
		stop()
		if !finished {
			select {
			case writeErr = <-done:
			case <-time.After(5 * time.Second):
				cancel()
				t.Fatal("member write did not join after releasing budget lock")
			}
		}
		requireReceiptNoError(t, releaseErr)
		requireReceiptCondition(t, writeErr == nil, "write failed after releasing lock", writeErr)
		t.Fatalf(
			"full-size zero-refund settlement waited for unrelated assigned-lane budget writer "+
				"(durable_append=%t)",
			durable,
		)
	}
}

func requireFullSizeCapacitySettlement(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	m *LocalMember,
	c claim,
	payload []byte,
	writeErr error,
	beforeCount, beforeBytes, beforeCharge int64,
) {
	t.Helper()
	var err error
	// Verify publication and bytes while the unrelated exclusive budget lock is
	// STILL held. No second replica or fallback can supply this original.
	requireReceiptNoError(t, writeErr)
	var raw []byte
	var reserved int64
	var settled bool
	err = f.pool.QueryRow(ctx, `SELECT locator,capacity_reserved_bytes,capacity_settled FROM fry_target WHERE `+
		`attempt=$1 AND member=$2`, c.Attempt.String(), m.id.String()).
		Scan(&raw, &reserved, &settled)
	requireReceiptNoError(t, err)
	actual, receiptErr8 := hotstore.EncodedRecordBytes(int64(len(payload)))
	requireReceiptNoError(t, receiptErr8)
	requireReceiptCondition(t, settled, "full-size target not durably settled")
	requireReceiptCondition(t, reserved == actual, "full-size target not durably settled")
	var loc hotstore.Locator
	err = json.Unmarshal(raw, &loc)
	requireReceiptNoError(t, err)
	var out bytes.Buffer
	{
		err = m.bytes.Read(ctx, loc, 0, int64(len(payload)), &out)
		requireReceiptCondition(t, err == nil, "full-size durable original differs", err)
		requireReceiptCondition(t, bytes.Equal(out.Bytes(), payload), "full-size durable original differs", err)
	}
	count, used, charge := capacityCountersContext(context.WithoutCancel(ctx), t, f, m)
	requireReceiptCondition(t, count == beforeCount, "zero-refund changed file budget")
	requireReceiptCondition(t, used == beforeBytes, "zero-refund changed file budget")
	requireReceiptCondition(t, charge == beforeCharge, "zero-refund changed file budget")
}

func capacityCountersContext(ctx context.Context, t *testing.T, f *fixture, m *LocalMember) (int64, int64, int64) {
	t.Helper()
	var records, bytes, charge int64
	requireReceiptNoError(
		t,
		f.pool.QueryRow(ctx, `SELECT `+
			`COALESCE(sum(l.reserved_records),0)::bigint,`+
			`(64+COALESCE(sum(l.reserved_bytes),0))::bigint,f.charged_bytes `+
			`FROM fry_member_file f LEFT JOIN fry_member_file_lane l `+
			`ON l.member=f.member AND l.file_id=f.file_id `+
			`WHERE f.member=$1 AND f.state='current' GROUP BY f.charged_bytes`, m.id.String()).
			Scan(&records, &bytes, &charge),
	)
	return records, bytes, charge
}

func requireCapacityRewriteSurvivorCensus(t *testing.T, f *fixture, next *LocalMember, c int64) {
	t.Helper()
	n, b, charge := capacityCounters(t, f, next)
	encoded, receiptErr3 := hotstore.EncodedRecordBytes(int64(len("live bounded original")))
	requireReceiptNoError(t, receiptErr3)
	requireReceiptCondition(t, n == 1, "survivor census charge wrong", n, b, charge)
	requireReceiptCondition(t, b == 64+encoded, "survivor census charge wrong", n, b, charge)
	requireReceiptCondition(t, charge == c, "survivor census charge wrong", n, b, charge)
}
