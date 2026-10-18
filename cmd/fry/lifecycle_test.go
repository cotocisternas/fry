//go:build linux

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/cotocisternas/fry/internal/objectstore"
	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/replicaconfig"
)

type reclaimVisit struct {
	member receiptstore.ID
	sealed bool
	rotate bool
}

type recordingReclaimer struct {
	visits []reclaimVisit
}

func (r *recordingReclaimer) ReclaimHot(_ context.Context, member receiptstore.ID) error {
	r.visits = append(r.visits, reclaimVisit{member: member})
	return receiptstore.ErrBusy
}

func (r *recordingReclaimer) ReclaimSealed(_ context.Context, member receiptstore.ID) error {
	r.visits = append(r.visits, reclaimVisit{member: member, sealed: true})
	return nil // Includes a member with no sealed candidates.
}

func (r *recordingReclaimer) RotateHotIfNeeded(_ context.Context, member receiptstore.ID) error {
	r.visits = append(r.visits, reclaimVisit{member: member, rotate: true})
	return receiptstore.ErrStale // A stale candidate must not prevent later work.
}

func TestAutomaticRotationSchedulingPreservesReclamationOpportunities(t *testing.T) {
	members := []receiptstore.ID{{1}, {2}}
	cursor := hotReclaimCursor{rotate: true}
	var recorder recordingReclaimer
	for range 12 {
		if err := cursor.run(
			t.Context(),
			&recorder,
			members,
		); err != nil && !errors.Is(err, receiptstore.ErrBusy) &&
			!errors.Is(err, receiptstore.ErrStale) {
			t.Fatal(err)
		}
	}
	if len(recorder.visits) != 12 {
		t.Fatalf("expected one action per opportunity, got %d", len(recorder.visits))
	}
	for _, member := range members {
		for _, kind := range []reclaimVisit{
			{member: member},
			{member: member, sealed: true},
			{member: member, rotate: true},
		} {
			requireVisitCount(t, recorder.visits, kind, 2)
		}
	}
}

func TestAutomaticRotationRequiresReclamation(t *testing.T) {
	_, err := startLifecycle(context.Background(), nil, &lifecycleConfig{RotateHot: true}, replicaconfig.Storage{})
	if err == nil || err.Error() != "automatic rotation requires hot reclamation" {
		t.Fatal("rotation without reclamation accepted", err)
	}
}

func TestHotReclamationSchedulesBothClassesDespiteBlockedAndEmptyMembers(t *testing.T) {
	members := []receiptstore.ID{{1}, {2}, {3}}
	var cursor hotReclaimCursor
	var recorder recordingReclaimer
	for range 12 {
		err := cursor.run(context.Background(), &recorder, members)
		if err != nil && !errors.Is(err, receiptstore.ErrBusy) {
			t.Fatal(err)
		}
	}
	for _, member := range members {
		requireMemberOpportunities(t, recorder.visits, member)
	}
	if len(recorder.visits) != 12 {
		t.Fatalf("expected one attempt per opportunity, got %d", len(recorder.visits))
	}
}

func TestBoundStorageCannotBeOmittedWithoutLiveReceipts(t *testing.T) {
	ctx := context.Background()
	var err error
	pool := commandDatabase(t, "fry_runtime_")
	checkCommand(t, receiptstore.Migrate(ctx, pool))
	if err = requireObjectConfiguration(ctx, pool, nil); err != nil {
		t.Fatalf("fresh hot-only storage: %v", err)
	}
	{
		_, err := pool.Exec(ctx, `UPDATE fry_object_namespace SET namespace='synthetic-bound-namespace'`)
		checkCommand(t, err)
	}
	if err = requireObjectConfiguration(ctx, pool, nil); err == nil {
		t.Fatal("bound storage silently disabled without live receipts")
	}
	if err = requireObjectConfiguration(ctx, pool, &lifecycleConfig{}); err == nil {
		t.Fatal("cleanup-only configuration silently discarded object obligations")
	}
	checkCommand(t, requireObjectConfiguration(ctx, pool, &lifecycleConfig{Objects: &objectstore.S3Config{}}))
	// Legacy unpublished upload history also fails closed even when no namespace
	// has been backfilled. Binding itself requires separate operator migration.
	if _, err = pool.Exec(ctx, "UPDATE fry_object_namespace SET namespace=NULL;\n INSERT INTO "+
		"fry_pack(id,organization,object_key,state,lease_until) "+
		"VALUES('old-pack','old-org','packs/v1/old','building',clock_timestamp())"); err != nil {
		t.Fatal(err)
	}
	if err = requireObjectConfiguration(ctx, pool, nil); err == nil {
		t.Fatal("unpublished upload obligations silently abandoned")
	}
}

func requireVisitCount(t *testing.T, visits []reclaimVisit, kind reclaimVisit, want int) {
	t.Helper()
	var count int
	for _, visit := range visits {
		if visit == kind {
			count++
		}
	}
	if count != want {
		t.Fatalf("work class/member %v received %d opportunities", kind, count)
	}
}

func requireMemberOpportunities(t *testing.T, visits []reclaimVisit, member receiptstore.ID) {
	t.Helper()
	var current, sealed int
	for _, visit := range visits {
		if visit.rotate {
			t.Fatal("rotation was not enabled")
		}
		if visit.member == member {
			if visit.sealed {
				sealed++
			} else {
				current++
			}
		}
	}
	if current != 2 || sealed != 2 {
		t.Fatalf("member %s opportunities: current=%d sealed=%d", member.String(), current, sealed)
	}
}
