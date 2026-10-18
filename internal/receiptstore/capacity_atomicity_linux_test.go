//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/cotocisternas/fry/internal/hotstore"
)

func TestCapacityTargetReservationFailureRollsBackWholePreparation(t *testing.T) {
	for _, kind := range []string{"exception", "suppressed"} {
		t.Run(kind, func(t *testing.T) {
			f := capacityFixture(t, 2)
			ctx := context.Background()
			attempt := newID(t)
			var before [2][3]int64
			for i, m := range f.members {
				before[i][0], before[i][1], before[i][2] = capacityCounters(t, f, m)
			}
			installCapacityReservationFault(t, ctx, f, kind, attempt)
			_, err := capacityAccept(ctx, f, attempt, f.envelope(t), "must never reach byte placement")
			if kind == "exception" {
				requireReceiptCondition(t, err != nil, "reservation stamp exception not exercised", err)
				requireReceiptCondition(
					t,
					strings.Contains(err.Error(), "synthetic reservation stamp failure"),
					"reservation stamp exception not exercised",
					err,
				)
			} else {
				requireReceiptCondition(
					t,
					errors.Is(err, ErrConflict),
					"suppressed reservation stamp not rejected",
					err,
				)
			}
			var invoked bool
			{
				e := f.pool.QueryRow(ctx, `SELECT is_called FROM capacity_reservation_fault_seen`).Scan(&invoked)
				requireReceiptCondition(t, e == nil, "reservation stamp trigger was not reached", e)
				requireReceiptCondition(t, invoked, "reservation stamp trigger was not reached", e)
			}

			for i, m := range f.members {
				n, b, c := capacityCounters(t, f, m)
				requireReceiptCondition(
					t,
					[3]int64{n, b, c} == before[i],
					"failed stamp left file debit",
					i,
					n,
					b,
					c,
					before[i],
				)
				requireReceiptCondition(t, len(m.bytes.Inventory()) == 0, "preparation failure dispatched bytes")
			}
			requireReceiptCondition(
				t,
				scalar(t, f.pool, `SELECT count(*) FROM fry_attempt WHERE id=$1`, attempt.String()) == 0,
				"failed preparation left partial authority",
			)
			requireReceiptCondition(
				t,
				scalar(t, f.pool, `SELECT count(*) FROM fry_target WHERE attempt=$1`, attempt.String()) == 0,
				"failed preparation left partial authority",
			)
		})
	}
}

func TestCapacityTargetSettlementFailureRollsBackRefundAndLocator(t *testing.T) {
	for _, kind := range []string{"exception", "suppressed"} {
		t.Run(kind, func(t *testing.T) {
			f := capacityFixture(t, 2)
			ctx := context.Background()
			c := prepare(t, f, newID(t), f.envelope(t))
			m := f.members[0]
			payload := []byte("durable short body with an uncommitted locator")
			beforeCount, beforeBytes, beforeCharge := capacityCounters(t, f, m)
			maxEncoded, receiptErr1 := hotstore.EncodedRecordBytes(m.opts.MaxMessageBytes)
			requireReceiptNoError(t, receiptErr1)
			installCapacitySettlementFault(t, ctx, f, kind, c, m)
			err := m.write(ctx, f.s.db, c, io.NopCloser(bytes.NewReader(payload)))
			if kind == "exception" {
				requireReceiptCondition(t, err != nil, "settlement stamp exception not exercised", err)
				requireReceiptCondition(
					t,
					strings.Contains(err.Error(), "synthetic settlement stamp failure"),
					"settlement stamp exception not exercised",
					err,
				)
			} else {
				requireReceiptCondition(t, errors.Is(err, ErrConflict), "suppressed target stamp not rejected", err)
			}
			var invoked bool
			{
				e := f.pool.QueryRow(ctx, `SELECT is_called FROM capacity_stamp_fault_seen`).Scan(&invoked)
				requireReceiptCondition(t, e == nil, "target stamp trigger was not reached", e)
				requireReceiptCondition(t, invoked, "target stamp trigger was not reached", e)
			}

			n, b, charge := capacityCounters(t, f, m)
			requireReceiptCondition(t, n == beforeCount, "failed target stamp committed partial refund", n, b, charge)
			requireReceiptCondition(t, b == beforeBytes, "failed target stamp committed partial refund", n, b, charge)
			requireReceiptCondition(
				t,
				charge == beforeCharge,
				"failed target stamp committed partial refund",
				n,
				b,
				charge,
			)
			requireUnpublishedSettlementBytes(t, ctx, f, c, m, maxEncoded, payload)
			requireCapacitySettlementReplay(
				t,
				f,
				c,
				m,
				payload,
				maxEncoded,
				beforeCount,
				beforeBytes,
				beforeCharge,
			)
		})
	}
}

func requireUnpublishedSettlementBytes(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	c claim,
	m *LocalMember,
	maxEncoded int64,
	payload []byte,
) {
	t.Helper()
	var err error
	var locator []byte
	var reserved int64
	var settled bool
	err = f.pool.QueryRow(ctx, `SELECT locator,capacity_reserved_bytes,capacity_settled FROM fry_target WHERE `+
		`attempt=$1 AND member=$2`, c.Attempt.String(), m.id.String()).
		Scan(&locator, &reserved, &settled)
	requireReceiptNoError(t, err)
	requireReceiptCondition(
		t,
		locator == nil,
		"failed settlement committed target state",
		len(locator),
		settled,
		reserved,
	)
	requireReceiptCondition(t, !settled, "failed settlement committed target state", len(locator), settled, reserved)
	requireReceiptCondition(
		t,
		reserved == maxEncoded,
		"failed settlement committed target state",
		len(locator),
		settled,
		reserved,
	)
	inventory := m.bytes.Inventory()
	requireReceiptCondition(t, len(inventory) == 1, "fixture must retain actual durable append")
	var out bytes.Buffer
	{
		err = m.bytes.Read(ctx, inventory[0], 0, int64(len(payload)), &out)
		requireReceiptCondition(t, err == nil, "unpublished durable bytes changed", err)
		requireReceiptCondition(t, bytes.Equal(out.Bytes(), payload), "unpublished durable bytes changed", err)
	}
}

func installCapacityReservationFault(t *testing.T, ctx context.Context, f *fixture, kind string, attempt ID) {
	t.Helper()
	timing, body := "AFTER", "RAISE EXCEPTION 'synthetic reservation stamp failure'"
	if kind == "suppressed" {
		timing, body = "BEFORE", "RETURN NULL"
	}
	_, err := f.pool.Exec(
		ctx,
		fmt.Sprintf(
			`CREATE SEQUENCE capacity_reservation_fault_seen; CREATE FUNCTION `+
				`reject_capacity_reservation_stamp() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN `+
				`PERFORM nextval('capacity_reservation_fault_seen'); %s; END$$; CREATE TRIGGER `+
				`reject_capacity_reservation_stamp %s UPDATE ON fry_target FOR EACH ROW WHEN `+
				`(NEW.attempt='%s' AND OLD.capacity_reserved_bytes IS NULL AND `+
				`NEW.capacity_reserved_bytes IS NOT NULL) EXECUTE FUNCTION `+
				`reject_capacity_reservation_stamp()`,
			body,
			timing,
			attempt.String(),
		),
	)
	requireReceiptNoError(t, err)
}

func installCapacitySettlementFault(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	kind string,
	c claim,
	m *LocalMember,
) {
	t.Helper()
	// A sequence is an explicit nontransactional test witness that the trigger
	// ran even when the target transaction rolls back.
	timing, body := "AFTER", "RAISE EXCEPTION 'synthetic settlement stamp failure'"
	if kind == "suppressed" {
		timing, body = "BEFORE", "RETURN NULL"
	}
	_, err := f.pool.Exec(
		ctx,
		fmt.Sprintf(
			`CREATE SEQUENCE capacity_stamp_fault_seen; CREATE FUNCTION `+
				`reject_capacity_settlement_stamp() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN `+
				`PERFORM nextval('capacity_stamp_fault_seen'); %s; END$$; CREATE TRIGGER `+
				`reject_capacity_settlement_stamp %s UPDATE ON fry_target FOR EACH ROW WHEN `+
				`(NEW.attempt='%s' AND NEW.member='%s' AND NOT OLD.capacity_settled AND `+
				`NEW.capacity_settled) EXECUTE FUNCTION reject_capacity_settlement_stamp()`,
			body,
			timing,
			c.Attempt.String(),
			m.id.String(),
		),
	)
	requireReceiptNoError(t, err)
}

func requireCapacitySettlementReplay(
	t *testing.T,
	f *fixture,
	c claim,
	m *LocalMember,
	payload []byte,
	maxEncoded, beforeCount, beforeBytes, beforeCharge int64,
) {
	t.Helper()
	ctx := context.Background()
	_, err := f.pool.Exec(ctx, `DROP TRIGGER reject_capacity_settlement_stamp ON fry_target`)
	requireReceiptNoError(t, err)
	for range 2 {
		{
			err = m.write(ctx, f.s.db, c, io.NopCloser(bytes.NewReader(payload)))
			requireReceiptCondition(t, err == nil, "copy replay failed", err)
		}
	}
	actual, receiptErr2 := hotstore.EncodedRecordBytes(int64(len(payload)))
	requireReceiptNoError(t, receiptErr2)
	n, b, charge := capacityCounters(t, f, m)
	requireReceiptCondition(t, n == beforeCount, "settlement replay refunded more than once", n, b, charge)
	requireReceiptCondition(
		t,
		b == beforeBytes-maxEncoded+actual,
		"settlement replay refunded more than once",
		n,
		b,
		charge,
	)
	requireReceiptCondition(
		t,
		charge == beforeCharge,
		"settlement replay refunded more than once",
		n,
		b,
		charge,
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE attempt=$1 AND member=$2 AND capacity_settled `+
				`AND capacity_reserved_bytes=$3 AND locator IS NOT NULL`,
			c.Attempt.String(),
			m.id.String(),
			actual,
		) == 1,
		"successful replay did not atomically publish locator and actual reservation",
	)
}
