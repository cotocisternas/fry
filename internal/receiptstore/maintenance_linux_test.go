//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
)

func maintenance(t *testing.T, f *fixture, p *Packer, actions, receipts int) *Maintenance {
	t.Helper()
	m, err := NewMaintenance(
		f.s,
		p,
		MaintenanceOptions{MaxActions: actions, MaxReceipts: receipts, MaxDuration: time.Minute},
	)
	requireReceiptNoError(t, err)
	return m
}

func runMaintenance(t *testing.T, m *Maintenance) MaintenanceResult {
	t.Helper()
	result, err := m.RunOnce(context.Background())
	requireReceiptCondition(t, err == nil, result, err)
	return result
}

func TestMaintenanceOptions(t *testing.T) {
	{
		_, err := NewMaintenance(nil, nil, MaintenanceOptions{MaxActions: 1, MaxReceipts: 1, MaxDuration: time.Second})
		requireReceiptCondition(t, err != nil, "nil store accepted")
	}
	f := setup(t)
	for _, opts := range []MaintenanceOptions{{0, 1, time.Second}, {1, 1025, time.Second}, {1, 1, 6 * time.Minute}} {
		{
			_, err := NewMaintenance(f.s, nil, opts)
			requireReceiptCondition(t, err != nil, "invalid bounds accepted", opts)
		}
	}
}

func TestMaintenancePackImageBoundsOrganizationTurnsAndOversized(t *testing.T) {
	f, p, _, _ := packing(t)
	p.opts.MaxPackBytes = 1024
	m := maintenance(t, f, p, 5, 2)
	// The older, larger organization sorts first, independent of random test IDs.
	big, small := f.org, newID(t)
	if big.String() > small.String() {
		big, small = small, big
	}
	env := f.envelope(t)
	env.Organization = big
	var bigReceipts []Receipt
	for range 4 {
		bigReceipts = append(bigReceipts, accept(t, f.s, newID(t), env, strings.Repeat("b", 700)))
	}
	// This older oversized receipt must not hide fitting candidates behind it.
	oversized := accept(t, f.s, newID(t), env, strings.Repeat("x", 2000))
	{
		_, err := f.pool.Exec(
			context.Background(),
			`UPDATE fry_receipt SET received_at=received_at-interval '1 day' WHERE id=$1`,
			oversized.ID.String(),
		)
		requireReceiptCondition(t, err == nil, err)
	}
	env.Organization = small
	smallReceipt := accept(t, f.s, newID(t), env, "small")
	requireMaintenanceOrganizationTurns(t, f, m, big, smallReceipt, oversized)
	for range 3 {
		runMaintenance(t, m)
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_receipt WHERE organization=$1 AND pack_id IS NOT NULL`,
			big.String(),
		) == 4,
		"bounded packing did not drain eligible backlog",
		bigReceipts,
	)
	// Nothing already packed should be selected again.
	{
		result := runMaintenance(t, m)
		requireReceiptCondition(t, result.Packed == 0, result)
	}
}

func TestMaintenanceInterruptedBuildRequiresClosedStagingAndActualPinClosure(t *testing.T) {
	f, p, _, _ := packing(t)
	r := accept(t, f.s, newID(t), f.envelope(t), "body")
	p.afterUpload = func(ID) error { return io.ErrUnexpectedEOF }
	job, err := p.Pack(context.Background(), f.org, []ID{r.ID})
	requireReceiptCondition(t, errors.Is(err, io.ErrUnexpectedEOF), job, err)
	m := maintenance(t, f, p, 5, 16)
	// A failed build with a future lease is neither repacked nor retired.
	result := runMaintenance(t, m)
	requireReceiptCondition(t, result.Packed == 0, result)
	requireReceiptCondition(t, result.Retired == 0, result)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_pack`) == 1,
		"unresolved receipt selected repeatedly",
	)
	ctx := context.Background()
	_, err = f.pool.Exec(
		ctx,
		`UPDATE fry_pack SET lease_until=clock_timestamp()-interval '1 `+
			`hour',staging_closed=false WHERE id=$1`,
		job.String(),
	)
	requireReceiptNoError(t, err)
	_, err = f.pool.Exec(
		ctx,
		`UPDATE fry_reference SET expires_at=clock_timestamp()-interval '1 hour' WHERE `+
			`receipt=$1`,
		r.ID.String(),
	)
	requireReceiptNoError(t, err)
	{
		result := runMaintenance(t, m)
		requireReceiptCondition(t, result.Retired == 0, result)
		requireReceiptCondition(t, result.Expired == 1, result)
		requireReceiptCondition(t, result.Finished == 0, result)
	}
	_, err = f.pool.Exec(ctx, `UPDATE fry_pack SET staging_closed=true WHERE id=$1`, job.String())
	requireReceiptNoError(t, err)
	pin := newID(t)
	_, err = f.pool.Exec(
		ctx,
		`INSERT INTO fry_read_pin(id,receipt,pack,deadline) `+
			`VALUES($1,$2,$3,clock_timestamp()-interval '1 hour')`,
		pin.String(),
		r.ID.String(),
		job.String(),
	)
	requireReceiptNoError(t, err)
	{
		result := runMaintenance(t, m)
		requireReceiptCondition(t, result.Retired == 0, "expired deadline treated as closure", result)
	}
	_, err = f.pool.Exec(ctx, `DELETE FROM fry_read_pin WHERE id=$1`, pin.String())
	requireReceiptNoError(t, err)
	{
		result := runMaintenance(t, m)
		requireReceiptCondition(t, result.Retired == 1, result)
		requireReceiptCondition(t, result.Finished == 0, result)
	}
	requireRetiredMaintenanceRetainsHotCopies(t, ctx, f, p, job, r)
}

func TestMaintenanceCompactsMixedLivePackAndRetiresItsSource(t *testing.T) {
	f, p, _, _ := packing(t)
	a := accept(t, f.s, newID(t), f.envelope(t), "keep")
	b := accept(t, f.s, newID(t), f.envelope(t), "expire")
	source, err := p.Pack(context.Background(), f.org, []ID{a.ID, b.ID})
	requireReceiptNoError(t, err)
	_, err = f.pool.Exec(
		context.Background(),
		`UPDATE fry_reference SET expires_at=clock_timestamp()-interval '1 hour' WHERE `+
			`receipt=$1`,
		b.ID.String(),
	)
	requireReceiptNoError(t, err)
	m := maintenance(t, f, p, 5, 16)
	first := runMaintenance(t, m)
	requireReceiptCondition(t, first.Expired == 1, first)
	requireReceiptCondition(t, first.Compacted == 1, first)
	requireReceiptCondition(t, first.Retired == 0, first)
	requireReceiptCondition(t, first.Packed == 0, first)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_receipt WHERE id=$1 AND pack_id IS NOT NULL AND pack_id!=$2`,
			a.ID.String(),
			source.String(),
		) == 1,
		"survivor not repacked",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_receipt WHERE id=$1 AND pack_id IS NULL`, b.ID.String()) == 1,
		"dead record remained in current pack",
	)
	second := runMaintenance(t, m)
	requireReceiptCondition(t, second.Retired == 1, second)
	requireReceiptCondition(t, second.Compacted == 0, second)
	{
		state, err := p.Status(context.Background(), f.org, source)
		requireReceiptCondition(t, err == nil, state, err)
		requireReceiptCondition(t, state == "retired", state, err)
	}
	err = f.s.Read(context.Background(), f.org, a.ID, 0, a.Size, io.Discard)
	requireReceiptNoError(t, err)
}

func TestMaintenanceOneActionRotationAndDeadline(t *testing.T) {
	f, p, _, _ := packing(t)
	accept(t, f.s, newID(t), f.envelope(t), "body")
	m := maintenance(t, f, p, 1, 1)
	for i := range 4 {
		{
			result := runMaintenance(t, m)
			requireReceiptCondition(t, result.Actions == 1, i, result)
			requireReceiptCondition(t, result.Packed == 0, i, result)
		}
	}
	{
		result := runMaintenance(t, m)
		requireReceiptCondition(t, result.Packed == 1, "action budget starved packing", result)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	{
		result, err := m.RunOnce(ctx)
		requireReceiptCondition(t, errors.Is(err, context.Canceled), result, err)
		requireReceiptCondition(t, result.Actions == 0, result, err)
	}
	// Closed packers cannot start work through maintenance.
	requireReceiptNoError(t, p.Close())
	accept(t, f.s, newID(t), f.envelope(t), "another")
	m.next = 4
	{
		_, err := m.RunOnce(context.Background())
		requireReceiptCondition(t, errors.Is(err, hotstore.ErrClosed), err)
	}
}

func TestMaintenancePartialErrorsAndBusyDoNotBlockOtherWork(t *testing.T) {
	f, p, _, objects := packing(t)
	dead := accept(t, f.s, newID(t), f.envelope(t), "expire")
	accept(t, f.s, newID(t), f.envelope(t), "pack")
	{
		_, err := f.pool.Exec(
			context.Background(),
			`UPDATE fry_reference SET expires_at=clock_timestamp()-interval '1 hour' WHERE `+
				`receipt=$1`,
			dead.ID.String(),
		)
		requireReceiptCondition(t, err == nil, err)
	}
	m := maintenance(t, f, p, 5, 16)
	failure := errors.New("object service unavailable")
	objects.create = func(context.Context, string, io.ReadSeeker, int64, [32]byte) error { return failure }
	result, err := m.RunOnce(context.Background())
	requireReceiptCondition(t, err != nil, "partial result lost", result, err)
	requireReceiptCondition(t, result.Expired == 1, "partial result lost", result, err)
	requireReceiptCondition(t, result.Packed == 0, "partial result lost", result, err)
	requireReceiptCondition(t, result.Actions == 5, "partial result lost", result, err)
	// A failed build remains unresolved and is not selected again on the next pass.
	{
		next := runMaintenance(t, m)
		requireReceiptCondition(t, next.Packed == 0, next)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_pack`) == 1,
		"failed build repeatedly selected",
	)
	objects.create = nil
	accept(t, f.s, newID(t), f.envelope(t), "next")
	// Busy construction is a normal skipped attempt, not a blocker for expiry.
	dead2 := accept(t, f.s, newID(t), f.envelope(t), "expire too")
	{
		_, err := f.pool.Exec(
			context.Background(),
			`UPDATE fry_reference SET expires_at=clock_timestamp()-interval '1 hour' WHERE `+
				`receipt=$1`,
			dead2.ID.String(),
		)
		requireReceiptCondition(t, err == nil, err)
	}
	p.slot <- struct{}{}
	busy, busyErr := m.RunOnce(context.Background())
	<-p.slot
	requireReceiptCondition(t, busyErr == nil, busy, busyErr)
	requireReceiptCondition(t, busy.Expired == 1, busy, busyErr)
	requireReceiptCondition(t, busy.Packed == 0, busy, busyErr)
	{
		next := runMaintenance(t, m)
		requireReceiptCondition(t, next.Packed == 1, next)
	}
}

func requireRetiredMaintenanceRetainsHotCopies(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	p *Packer,
	job ID,
	r Receipt,
) {
	t.Helper()
	{
		state, err := p.Status(ctx, f.org, job)
		requireReceiptCondition(t, err == nil, state, err)
		requireReceiptCondition(t, state == "retired", state, err)
	}
	// Maintenance does not erase hot copies or claim byte cleanup completion.
	for _, member := range f.members {
		requireReceiptCondition(t, len(member.bytes.Inventory()) == 1, "hot bytes reclaimed")
	}
	{
		state, err := f.s.CleanupStatus(ctx, f.org, r.ID)
		requireReceiptCondition(t, err == nil, state, err)
		requireReceiptCondition(t, state.State == "retiring", state, err)
	}
}

func requireMaintenanceOrganizationTurns(
	t *testing.T,
	f *fixture,
	m *Maintenance,
	big ID,
	smallReceipt, oversized Receipt,
) {
	t.Helper()
	first := runMaintenance(t, m)
	requireReceiptCondition(t, first.Packed == 1, first)
	requireReceiptCondition(t, first.Oversized == 1, first)
	requireReceiptCondition(t, first.Actions == 5, first)
	second := runMaintenance(t, m)
	requireReceiptCondition(t, second.Packed == 1, second)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_receipt WHERE id=$1 AND pack_id IS NOT NULL`,
			smallReceipt.ID.String(),
		) == 1,
		"large organization starved the small organization",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_receipt WHERE organization=$1 AND pack_id IS NOT NULL`,
			big.String(),
		) == 1,
		"second turn returned to large organization",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_pack WHERE image_size>1024`) == 0,
		"raw byte bound ignored image framing",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_receipt WHERE id=$1 AND pack_id IS NOT NULL`,
			oversized.ID.String(),
		) == 0,
		"oversized receipt selected",
	)
}
