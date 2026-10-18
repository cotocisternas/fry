//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"testing"
)

func TestClosedMemberReconciliationRequiresProofAndPreservesLiveGeneration(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	members, servers := remoteFixture(t, f)
	s, err := NewWithMembers(ctx, f.pool, [2]Member{members[0], members[1]}, f.s.db.opts)
	requireReceiptNoError(t, err)
	receipt := accept(t, s, newID(t), f.envelope(t), "retained bytes")
	execSQL(
		t,
		f.pool,
		`CREATE FUNCTION fail_closed_read() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE `+
			`EXCEPTION 'fixture closure failure'; END $$`,
	)
	execSQL(
		t,
		f.pool,
		`CREATE TRIGGER fail_closed_read BEFORE UPDATE ON fry_member_read FOR EACH ROW EXECUTE `+
			`FUNCTION fail_closed_read()`,
	)
	var output bytes.Buffer
	if err = s.Read(ctx, f.org, receipt.ID, 0, receipt.Size, &output); err == nil {
		t.Fatal("expected missing closure proof")
	}
	execSQL(t, f.pool, `DROP TRIGGER fail_closed_read ON fry_member_read`)
	if n, err := s.ReconcileMemberClosures(ctx, 10); err != nil || n != 0 {
		t.Fatal("live generation reconciled", n, err)
	}
	err = servers[0].Close(ctx)
	requireReceiptNoError(t, err)
	if n, err := s.ReconcileMemberClosures(ctx, 1); err != nil || n != 1 {
		t.Fatal("closed generation not reconciled", n, err)
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_read_pin WHERE owner_member=$1`,
			members[1].descriptor.Member.String(),
		) == 1,
		"other generation's pin released",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member_read WHERE closed`) == 1,
		"read operation identity not retained",
	)
	err = servers[1].Close(ctx)
	requireReceiptNoError(t, err)
	if n, err := s.ReconcileMemberClosures(ctx, 10); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if n, err := s.ReconcileMemberClosures(ctx, 10); err != nil || n != 0 {
		t.Fatal("not idempotent", n, err)
	}
}

func TestClosedMemberReconciliationConfirmsAbortedTargetsOnly(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	members, servers := remoteFixture(t, f)
	s, err := NewWithMembers(ctx, f.pool, [2]Member{members[0], members[1]}, f.s.db.opts)
	requireReceiptNoError(t, err)
	envelope, data, err := normalize(f.envelope(t))
	requireReceiptNoError(t, err)
	attempt := newID(t)
	_, _, err = s.db.prepareMembers(
		ctx,
		attempt,
		envelope,
		data,
		[2]MemberIdentity{members[0].identity(), members[1].identity()},
	)
	requireReceiptNoError(t, err)
	err = servers[0].Close(ctx)
	requireReceiptNoError(t, err)
	if n, err := s.ReconcileMemberClosures(ctx, 10); err != nil || n != 0 {
		t.Fatal("preparing target canceled", n, err)
	}
	err = s.db.abort(ctx, f.org, attempt)
	requireReceiptNoError(t, err)
	if n, err := s.ReconcileMemberClosures(ctx, 10); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_cleanup WHERE cancel_confirmed AND NOT bytes_removed`) == 1,
		"closure confused with erasure",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_target WHERE cancel_confirmed`) == 1,
		"live target canceled",
	)
}
