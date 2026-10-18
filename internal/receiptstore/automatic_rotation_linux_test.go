//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
)

func automaticFixture(t *testing.T, records int64) *fixture {
	t.Helper()
	o := capacityOptions(records)
	o.MaxBytes += 4 * o.MaxFileBytes
	return setupWithPoolConfig(t, nil, o, Options{Lease: 10 * time.Second, RecoveryRetention: time.Second})
}

func TestAutomaticRotationFullFileAdvancesOnlyThroughMaintenance(t *testing.T) {
	f := automaticFixture(t, 1)
	ctx := context.Background()
	body := strings.Repeat("x", 1024)
	original := accept(t, f.s, newID(t), f.envelope(t), body)
	for _, m := range f.members {
		old := m.bytes.FileID()
		requireReceiptNoError(t, f.s.RotateHotIfNeeded(ctx, m.id))
		requireReceiptCondition(t, m.bytes.FileID() != old, "full file not rotated")
		requireReceiptCondition(
			t,
			scalar(
				t,
				f.pool,
				`SELECT count(*) FROM fry_target WHERE member=$1 AND attempt=$2 AND incarnation=$3 AND `+
					`NOT bytes_removed`,
				m.id.String(),
				original.Attempt.String(),
				old.String(),
			) == 1,
			"automatic rotation retargeted original",
		)
	}
	accept(t, f.s, newID(t), f.envelope(t), "new automatic current")
	var got bytes.Buffer
	{
		err := f.s.Read(ctx, f.org, original.ID, 0, original.Size, &got)
		requireReceiptCondition(t, err == nil, "historical bytes", err)
		requireReceiptCondition(t, got.String() == body, "historical bytes", err)
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_maintenance WHERE kind='auto-rotate' AND `+
				`state='complete'`,
		) == 2,
		"conditional maintenance provenance missing",
	)
}

func TestAutomaticRotationFitsAndLegacyAreNoops(t *testing.T) {
	for _, bounded := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "bounded"}[bounded], func(t *testing.T) {
			var f *fixture
			if bounded {
				f = automaticFixture(t, 2)
			} else {
				f = setup(t)
			}
			ctx := context.Background()
			accept(t, f.s, newID(t), f.envelope(t), "room remains")
			m := f.members[0]
			old := m.bytes.FileID()
			requireReceiptNoError(t, f.s.RotateHotIfNeeded(ctx, m.id))
			requireReceiptCondition(t, m.bytes.FileID() == old, "fitting file rotated")
			requireReceiptCondition(
				t,
				scalar(t, f.pool, `SELECT count(*) FROM fry_hot_rewrite WHERE member=$1`, m.id.String()) == 0,
				"fitting file rotated",
			)
			requireReceiptCondition(
				t,
				scalar(t, f.pool, `SELECT count(*) FROM fry_member WHERE maintenance_job IS NOT NULL`) == 0,
				"noop admission gate remains",
			)
		})
	}
}

func TestAutomaticRotationInsufficientSparePreservesBackpressure(t *testing.T) {
	f := capacityFixture(t, 1)
	ctx := context.Background()
	accept(t, f.s, newID(t), f.envelope(t), strings.Repeat("x", 1024))
	m := f.members[0]
	old := m.bytes.FileID()
	{
		err := f.s.RotateHotIfNeeded(ctx, m.id)
		requireReceiptCondition(t, errors.Is(err, hotstore.ErrCapacity), err)
	}
	requireReceiptCondition(t, old == m.bytes.FileID(), "known capacity failure changed/froze current")
	requireReceiptCondition(t, !m.isFrozen(), "known capacity failure changed/froze current")
	{
		_, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "still full")
		requireReceiptCondition(t, errors.Is(err, hotstore.ErrCapacity), "capacity rejection lost", err)
	}
}

func TestAutomaticRotationRemoteStaleCacheRefreshesWithoutIdentityRequest(t *testing.T) {
	f := automaticFixture(t, 2)
	ctx := context.Background()
	var identities atomic.Int64
	wrapper := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/identity" {
				identities.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	}
	s, members, _ := remoteMaintenance(t, f, wrapper)
	stale := cachedCoordinator(t, f, members)
	accept(t, s, newID(t), f.envelope(t), "source before another coordinator")
	requireReceiptNoError(t, s.RotateHot(ctx, members[0].identity().Member))
	before := identities.Load()
	cached := receiptTestValue[*RemoteMember](t, stale.members[0])
	{
		err := stale.RotateHotIfNeeded(ctx, cached.identity().Member)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "stale call not rejected", err)
	}
	requireReceiptCondition(
		t,
		cached.identity().Incarnation == members[0].identity().Incarnation,
		"stale response did not update physical cache",
	)
	{
		err := stale.RotateHotIfNeeded(ctx, cached.identity().Member)
		requireReceiptCondition(t, err == nil, "next turn did not progress", err)
	}
	requireReceiptCondition(t, identities.Load() == before, "conditional operation performed an identity RPC")
}

func TestAutomaticRotationRemoteRefreshContentionIsBusy(t *testing.T) {
	f := automaticFixture(t, 1)
	ctx := context.Background()
	_, members, _ := remoteMaintenance(t, f)
	members[0].refreshMu.Lock()
	defer members[0].refreshMu.Unlock()
	{
		err := members[0].RotateHotIfNeeded(ctx, newID(t))
		requireReceiptCondition(t, errors.Is(err, ErrBusy), "queued behind refresh", err)
	}
}

func TestAutomaticRotationNoopReplayNeverReevaluatesLaterFullFile(t *testing.T) {
	f := automaticFixture(t, 1)
	ctx := context.Background()
	s, members, _ := remoteMaintenance(t, f)
	operation := newID(t)
	requireReceiptNoError(t, members[0].RotateHotIfNeeded(ctx, operation))
	accept(t, s, newID(t), f.envelope(t), strings.Repeat("x", 1024))
	old := members[0].identity().Incarnation
	requireReceiptNoError(t, members[0].RotateHotIfNeeded(ctx, operation))
	requireReceiptCondition(t, members[0].identity().Incarnation == old, "completed no-op replay rotated")
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_hot_rewrite WHERE member=$1`,
			members[0].identity().Member.String(),
		) == 0,
		"completed no-op replay rotated",
	)
	{
		state, err := members[0].AutomaticRotationStatus(ctx, operation)
		requireReceiptCondition(t, err == nil, state, err)
		requireReceiptCondition(t, state == "complete", state, err)
	}
	{
		_, err := members[0].RotationStatus(ctx, operation)
		requireReceiptCondition(t, err != nil, "explicit/automatic status kinds confused")
	}
	requireReceiptNoError(t, members[0].RotateHotIfNeeded(ctx, newID(t)))
	changed := members[0].identity().Incarnation
	requireReceiptCondition(t, changed != old, "new conditional operation did not rotate")
	{
		err := members[0].RotateHotIfNeeded(ctx, operation)
		requireReceiptCondition(t, err == nil, "old completed op after later rotation", err)
	}
	requireReceiptCondition(t, members[0].identity().Incarnation == changed, "old completed op retargeted current")
}

func TestAutomaticRotationPreparingReservationsKeepBackpressure(t *testing.T) {
	for _, nonempty := range []bool{false, true} {
		t.Run(fmt.Sprint(nonempty), func(t *testing.T) {
			records := int64(1)
			if nonempty {
				records = 2
			}
			f := automaticFixture(t, records)
			ctx := context.Background()
			m := f.members[0]
			if nonempty {
				accept(t, f.s, newID(t), f.envelope(t), "existing physical original")
			}
			claim := prepare(t, f, newID(t), f.envelope(t))
			old := m.bytes.FileID()
			beforeRecords, beforeBytes, _ := capacityCounters(t, f, m)
			{
				err := f.s.RotateHotIfNeeded(ctx, m.id)
				requireReceiptCondition(t, errors.Is(err, ErrBusy), "preparing reservation rotated/nooped", err)
			}
			afterRecords, afterBytes, _ := capacityCounters(t, f, m)
			requireReceiptCondition(t, m.bytes.FileID() == old, "preparing target lost capacity/identity")
			requireReceiptCondition(t, beforeRecords == afterRecords, "preparing target lost capacity/identity")
			requireReceiptCondition(t, beforeBytes == afterBytes, "preparing target lost capacity/identity")
			requireReceiptCondition(
				t,
				scalar(t, f.pool, `SELECT count(*) FROM fry_hot_rewrite WHERE member=$1`, m.id.String()) == 0,
				"preparing target created rotation job",
			)
			requireReceiptCondition(
				t,
				scalar(
					t,
					f.pool,
					`SELECT count(*) FROM fry_attempt WHERE id=$1 AND state='preparing'`,
					claim.Attempt.String(),
				) == 1,
				"automatic operation changed attempt authority",
			)
		})
	}
}

func TestAutomaticRotationEmptyClosedAbortNoopRetainsDebt(t *testing.T) {
	f := automaticFixture(t, 1)
	ctx := context.Background()
	m := f.members[0]
	attempt := newID(t)
	prepare(t, f, attempt, f.envelope(t))
	requireReceiptNoError(t, f.s.Abort(ctx, f.org, attempt))
	old := m.bytes.FileID()
	beforeRecords, beforeBytes, _ := capacityCounters(t, f, m)
	requireReceiptNoError(t, f.s.RotateHotIfNeeded(ctx, m.id))
	afterRecords, afterBytes, _ := capacityCounters(t, f, m)
	requireReceiptCondition(t, m.bytes.FileID() == old, "empty closed abort dropped reservation")
	requireReceiptCondition(t, beforeRecords == afterRecords, "empty closed abort dropped reservation")
	requireReceiptCondition(t, beforeBytes == afterBytes, "empty closed abort dropped reservation")
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_hot_rewrite WHERE member=$1`, m.id.String()) == 0,
		"empty source created successor",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE member=$1 AND attempt=$2 AND cancel_confirmed `+
				`AND NOT bytes_removed AND locator IS NULL`,
			m.id.String(),
			attempt.String(),
		) == 1,
		"closed abort debt changed",
	)
}

func TestAutomaticRotationRemoteOperationKindAndOwnerBinding(t *testing.T) {
	f := automaticFixture(t, 1)
	ctx := context.Background()
	_, members, servers := remoteMaintenance(t, f)
	operation := newID(t)
	requireReceiptNoError(t, members[0].RotateHotIfNeeded(ctx, operation))
	{
		err := members[0].RotateHot(ctx, operation)
		requireReceiptCondition(t, err != nil, "automatic operation reused as explicit")
	}
	{
		err := members[0].ReclaimSealed(ctx, operation)
		requireReceiptCondition(t, err != nil, "automatic operation reused as sealed reclaim")
	}
	m := f.members[0]
	oldEpoch := servers[0].owner.epoch
	requireReceiptNoError(t, servers[0].Close(ctx))
	next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, m.opts)
	requireReceiptNoError(t, err)
	copyFixture := *f
	copyFixture.members[0] = next
	server, certs, url := memberHTTP(t, &copyFixture)
	requireReceiptCondition(t, server.owner.epoch != oldEpoch, "owner epoch unchanged")
	for _, epoch := range []int64{oldEpoch, server.owner.epoch} {
		data, receiptErr1 := json.Marshal(
			memberRequest{Operation: operation, OwnerEpoch: epoch, Incarnation: next.bytes.FileID()},
		)
		requireReceiptNoError(t, receiptErr1)
		for _, path := range []string{"/v1/rotate-if-needed", "/v1/rotate-if-needed/status"} {
			response, err := receiptTestRequest(t, certs.allowed, http.MethodPost, url+path, bytes.NewReader(data))
			requireReceiptNoError(t, err)
			if epoch == oldEpoch || strings.HasSuffix(path, "/status") {
				if err := response.Body.Close(); err != nil {
					t.Error(err)
				}
				requireReceiptCondition(t, response.StatusCode == http.StatusConflict, path, epoch, response.Status)
				continue
			}
			var result automaticRotationResponse
			err = decodeMemberJSON(response, &result)
			requireReceiptNoError(t, err)
			requireReceiptCondition(t, result.State == "conflict", "old operation rebound to new owner", result)
			requireReceiptCondition(t, result.Identity == nil, "old operation rebound to new owner", result)
		}
	}
}
