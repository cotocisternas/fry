//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

type heldReadWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	bytes.Buffer
}

func (w *heldReadWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.Buffer.Write(p)
}

func TestMemberReadPinSurvivesCoordinatorHandleClosure(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r := accept(t, f.s, newID(t), f.envelope(t), "original member bytes")
	owner, err := ownMember(ctx, f.members[0])
	requireReceiptNoError(t, err)
	parent, err := f.s.Open(ctx, f.org, r.ID, 0, r.Size)
	requireReceiptNoError(t, err)
	defer func() {
		if err := parent.Close(); err != nil {
			t.Error(err)
		}
	}()
	var parentID string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(ctx, `SELECT id FROM fry_read_pin WHERE receipt=$1 AND owner_member IS NULL`, r.ID.String()).
			Scan(&parentID),
	)
	pin, err := idFrom(parentID)
	requireReceiptNoError(t, err)
	defer func() {
		if err := parent.Close(); err != nil {
			t.Error(err)
		}
		if err := owner.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	writer := &heldReadWriter{entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(writer.release) })
	operation := newID(t)
	done := make(chan error, 1)
	go func() {
		done <- owner.read(ctx, operation, pin, f.org, r.ID, f.members[0].bytes.FileID(), 0, r.Size, writer)
	}()
	select {
	case <-writer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("member read did not begin")
	}
	if err := parent.Close(); err != nil {
		t.Error(err)
	}
	requireMemberReadRetainsGenerationPin(t, f, r, operation, owner)
	release.Do(func() { close(writer.release) })
	requireReceiptNoError(t, <-done)
	if writer.String() != "original member bytes" {
		t.Fatal(writer.String())
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin WHERE receipt=$1`, r.ID.String()) == 0,
		"completed read still pinned",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member_read WHERE id=$1 AND closed`, operation.String()) == 1,
		"missing completion proof",
	)
}

func TestMemberReadMissingClosureProofRetainsPin(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r := accept(t, f.s, newID(t), f.envelope(t), "original")
	owner, err := ownMember(ctx, f.members[0])
	requireReceiptNoError(t, err)
	parent, err := f.s.Open(ctx, f.org, r.ID, 0, r.Size)
	requireReceiptNoError(t, err)
	defer func() {
		if err := parent.Close(); err != nil {
			t.Error(err)
		}
	}()
	var parentID string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(ctx, `SELECT id FROM fry_read_pin WHERE receipt=$1`, r.ID.String()).Scan(&parentID),
	)
	pin, err := idFrom(parentID)
	requireReceiptNoError(t, err)
	defer func() {
		if err := parent.Close(); err != nil {
			t.Error(err)
		}
		if err := owner.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	execSQL(
		t,
		f.pool,
		`CREATE FUNCTION fail_read_closure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN `+
			`RAISE EXCEPTION 'fixture closure failure'; END $$`,
	)
	execSQL(
		t,
		f.pool,
		`CREATE TRIGGER fail_read_closure BEFORE UPDATE ON fry_member_read FOR EACH ROW `+
			`EXECUTE FUNCTION fail_read_closure()`,
	)
	operation := newID(t)
	var bytes bytes.Buffer
	err = owner.read(ctx, operation, pin, f.org, r.ID, f.members[0].bytes.FileID(), 0, r.Size, &bytes)
	if err == nil || bytes.String() != "original" {
		t.Fatal("closure failure hidden", bytes.String(), err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin WHERE id=$1`, operation.String()) == 1 &&
			scalar(
				t,
				f.pool,
				`SELECT count(*) FROM fry_member_read WHERE id=$1 AND NOT closed`,
				operation.String(),
			) == 1,
		"unconfirmed closure discarded",
	)
}

func requireMemberReadRetainsGenerationPin(t *testing.T, f *fixture, r Receipt, operation ID, owner *memberOwner) {
	t.Helper()
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin WHERE receipt=$1`, r.ID.String()) == 1,
		"coordinator close removed member's protection",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_read_pin WHERE id=$1 AND owner_member=$2 AND owner_epoch=$3`,
			operation.String(),
			owner.member.id.String(),
			owner.epoch,
		) == 1,
		"missing generation-scoped pin",
	)
}
