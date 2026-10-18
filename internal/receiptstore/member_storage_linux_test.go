//go:build linux

package receiptstore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMemberStorageBoundedSnapshotAndPage(t *testing.T) {
	f := automaticFixture(t, 2)
	ctx := context.Background()
	m := f.members[0]
	body := "diagnostic original"
	accept(t, f.s, newID(t), f.envelope(t), body)
	snapshot, err := f.s.MemberStorage(ctx, m.id, ID{}, 1)
	requireReceiptNoError(t, err)
	requireBoundedStorageProfile(t, snapshot, m)
	requireStorageReservation(t, snapshot, body)
	requireEmbeddedStorageRuntime(t, snapshot, m)
	accept(t, f.s, newID(t), f.envelope(t), "second diagnostic original")
	full, err := f.s.MemberStorage(ctx, m.id, ID{}, 64)
	requireReceiptNoError(t, err)
	requireReceiptCondition(
		t,
		full.Current.CanFitMaxRecordSnapshot != nil && !*full.Current.CanFitMaxRecordSnapshot,
		"full record reservations reported fit",
	)
	err = f.s.RotateHot(ctx, m.id)
	requireReceiptNoError(t, err)
	ids := storagePageIDs(t, ctx, f, m)
	requireReceiptCondition(
		t,
		len(ids) == 2 && sort.StringsAreSorted(ids) && ids[0] != ids[1],
		"keyset skipped/duplicated catalog rows",
		ids,
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member_file WHERE member=$1`, m.id.String()) == int64(len(ids)),
		"catalog page lost history",
	)
	requireStorageJSONRepresentation(t, snapshot, m.root)
}

func TestMemberStorageLegacyUnknownAndBounds(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	m := f.members[0]
	snapshot, err := f.s.MemberStorage(ctx, m.id, ID{}, 64)
	requireReceiptNoError(t, err)
	requireReceiptCondition(
		t,
		!snapshot.Profile.Bounded,
		"legacy registration evidence presented as live bounded authority",
		snapshot,
	)
	requireReceiptCondition(
		t,
		!snapshot.Profile.LimitsFrozen,
		"legacy registration evidence presented as live bounded authority",
		snapshot,
	)
	requireReceiptCondition(
		t,
		snapshot.Current.ReservedRecords == nil,
		"legacy registration evidence presented as live bounded authority",
		snapshot,
	)
	requireReceiptCondition(
		t,
		snapshot.Current.ReservedRecordBytes == nil,
		"legacy registration evidence presented as live bounded authority",
		snapshot,
	)
	requireReceiptCondition(
		t,
		snapshot.Current.CanFitMaxRecordSnapshot == nil,
		"legacy registration evidence presented as live bounded authority",
		snapshot,
	)
	requireReceiptCondition(
		t,
		snapshot.Current.Lanes != nil,
		"legacy registration evidence presented as live bounded authority",
		snapshot,
	)
	requireReceiptCondition(
		t,
		len(snapshot.Current.Lanes) == 0,
		"legacy registration evidence presented as live bounded authority",
		snapshot,
	)
	for _, limit := range []int{0, -1, 65} {
		if _, err = f.s.MemberStorage(ctx, m.id, ID{}, limit); err == nil {
			t.Fatal("invalid page limit accepted", limit)
		}
	}
	if _, err = f.s.MemberStorage(ctx, newID(t), ID{}, 64); !errors.Is(err, ErrNotFound) {
		t.Fatal("unconfigured member queried", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = f.s.MemberStorage(canceled, m.id, ID{}, 64); err == nil {
		t.Fatal("canceled metadata snapshot succeeded")
	}
}

func TestMemberStorageReleasesPoolBeforeOwnerRPC(t *testing.T) {
	opts := capacityOptions(2)
	opts.MaxBytes += 4 * opts.MaxFileBytes
	// Owner connections are dedicated pgx connections outside this pool. Its
	// single checkout must be released before the runtime handler's probe.
	f := setupWithPoolConfig(
		t,
		func(c *pgxpool.Config) { c.MaxConns = 1 },
		opts,
		Options{Lease: 10 * time.Second, RecoveryRetention: time.Second},
	)
	var probed atomic.Bool
	wrapper := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/storage-status" {
				var one int
				if err := f.pool.QueryRow(r.Context(), `SELECT 1`).Scan(&one); err != nil {
					http.Error(w, "probe unavailable", http.StatusServiceUnavailable)
					return
				}
				probed.Store(one == 1)
			}
			next.ServeHTTP(w, r)
		})
	}
	s, members, _ := remoteMaintenance(t, f, wrapper)
	snapshot, err := s.MemberStorage(context.Background(), members[0].identity().Member, ID{}, 64)
	requireReceiptNoError(t, err)
	if !probed.Load() || snapshot.Runtime.Status != "sampled" {
		t.Fatal("pool remained held across runtime probe", snapshot.Runtime)
	}
	requireReceiptCondition(
		t,
		snapshot.Owner.Epoch != nil && snapshot.Owner.Closed != nil && !*snapshot.Owner.Closed,
		"owner metadata not sampled",
	)
}

func requireBoundedStorageProfile(t *testing.T, snapshot MemberStorageSnapshot, m *LocalMember) {
	t.Helper()
	if !snapshot.Profile.Bounded || !snapshot.Profile.LimitsFrozen ||
		snapshot.Profile.MaxFileBytes != storageDecimal(m.opts.MaxFileBytes) {
		t.Fatal("wrong frozen profile", snapshot.Profile)
	}
}

func requireStorageReservation(t *testing.T, snapshot MemberStorageSnapshot, body string) {
	t.Helper()
	encoded, receiptErr1 := hotstore.EncodedRecordBytes(int64(len(body)))
	requireReceiptNoError(t, receiptErr1)
	if snapshot.Current.ReservedRecords == nil || *snapshot.Current.ReservedRecords != "1" ||
		snapshot.Current.ReservedRecordBytes == nil ||
		*snapshot.Current.ReservedRecordBytes != storageDecimal(encoded) ||
		snapshot.Current.CanFitMaxRecordSnapshot == nil ||
		!*snapshot.Current.CanFitMaxRecordSnapshot {
		t.Fatal("reservation snapshot does not match real accepted bytes", snapshot.Current)
	}
}

func requireEmbeddedStorageRuntime(t *testing.T, snapshot MemberStorageSnapshot, m *LocalMember) {
	t.Helper()
	requireReceiptCondition(t, !snapshot.SQLSnapshotAt.IsZero(), "missing SQL snapshot time")
	runtime := snapshot.Runtime
	requireReceiptCondition(t, runtime.Status == "sampled", "runtime not sampled", runtime)
	requireReceiptCondition(t, runtime.SampledAt != nil, "missing runtime sample time", runtime)
	requireReceiptCondition(t, runtime.Closed != nil, "unknown closure", runtime)
	requireReceiptCondition(t, !*runtime.Closed, "closed runtime", runtime)
	requireReceiptCondition(t, runtime.Frozen != nil, "unknown frozen state", runtime)
	requireReceiptCondition(t, !*runtime.Frozen, "frozen runtime", runtime)
	requireReceiptCondition(t, runtime.CurrentFile != nil, "unknown current file", runtime)
	requireReceiptCondition(t, *runtime.CurrentFile == m.bytes.FileID().String(), "wrong current file", runtime)
	requireReceiptCondition(t, snapshot.Owner.Epoch == nil, "embedded member fabricated process epoch")
	requireReceiptCondition(t, snapshot.Owner.Closed == nil, "embedded member fabricated process closure")
}

func storagePageIDs(t *testing.T, ctx context.Context, f *fixture, m *LocalMember) []string {
	t.Helper()
	var ids []string
	var cursor ID
	for range 3 {
		page, e := f.s.MemberStorage(ctx, m.id, cursor, 1)
		requireReceiptNoError(t, e)
		if len(page.Files) != 1 {
			t.Fatal("unexpected page size", len(page.Files))
		}
		ids = append(ids, page.Files[0].FileID)
		if page.NextFile == nil {
			break
		}
		cursor, e = idFrom(*page.NextFile)
		requireReceiptNoError(t, e)
	}
	return ids
}

func requireStorageJSONRepresentation(t *testing.T, snapshot MemberStorageSnapshot, root string) {
	t.Helper()
	data, err := json.Marshal(snapshot)
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, !strings.Contains(string(data), root), "diagnostic leaked filesystem path")
	var raw map[string]any
	err = json.Unmarshal(data, &raw)
	requireReceiptNoError(t, err)
	if _, ok := raw["member"].(string); !ok {
		t.Fatal("member ID is not JSON string")
	}
	profile := receiptTestValue[map[string]any](t, raw["profile"])
	if _, ok := profile["max_bytes"].(string); !ok {
		t.Fatal("64-bit count not decimal JSON string")
	}
}
