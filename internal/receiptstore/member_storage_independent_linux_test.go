//go:build linux

package receiptstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func requireStorageRuntimeUnknown(t *testing.T, runtime MemberStorageRuntime) {
	t.Helper()
	requireReceiptCondition(t, runtime.SampledAt == nil, "unsampled runtime exposed guarded state", runtime)
	requireReceiptCondition(t, runtime.CurrentFile == nil, "unsampled runtime exposed guarded state", runtime)
	requireReceiptCondition(t, runtime.Closed == nil, "unsampled runtime exposed guarded state", runtime)
	requireReceiptCondition(t, runtime.Frozen == nil, "unsampled runtime exposed guarded state", runtime)
	requireReceiptCondition(t, runtime.ClosureUncertain == nil, "unsampled runtime exposed guarded state", runtime)
}

// Compare durable authority, not just the returned diagnostic fields. The
// fixture has no background mutation of these rows while this helper runs.
func storageDiagnosticAuthority(t *testing.T, f *fixture, member ID) string {
	t.Helper()
	var result string
	err := f.pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
 'member',(SELECT to_jsonb(m) FROM fry_member m WHERE id=$1),
 'files',(SELECT jsonb_agg(to_jsonb(f) ORDER BY file_id) FROM fry_member_file f WHERE member=$1),
 'lanes',(SELECT jsonb_agg(to_jsonb(l) ORDER BY file_id,lane) FROM fry_member_file_lane l WHERE member=$1),
 'owners',(SELECT jsonb_agg(to_jsonb(o) ORDER BY epoch) FROM fry_member_owner o WHERE member=$1),
 'jobs',(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM fry_hot_rewrite j WHERE member=$1),
 'maintenance',(SELECT jsonb_agg(to_jsonb(k) ORDER BY id) FROM fry_member_maintenance k WHERE member=$1),
 'targets',(SELECT jsonb_agg(to_jsonb(t) ORDER BY attempt) FROM fry_target t WHERE member=$1)
)::text`, member.String()).Scan(&result)
	requireReceiptNoError(t, err)
	return result
}

func TestMemberStorageIndependentBusyGateLeavesRuntimeUnknown(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprintf("remote=%t", remote), func(t *testing.T) {
			f := capacityFixture(t, 4)
			store := f.s
			if remote {
				store, _, _ = remoteMaintenance(t, f)
			}
			m := f.members[0]
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			type outcome struct {
				value MemberStorageSnapshot
				err   error
			}
			done := make(chan outcome, 1)
			m.gate.Lock()
			go func() {
				value, err := store.MemberStorage(ctx, m.id, ID{}, 1)
				done <- outcome{value, err}
			}()
			var result outcome
			prompt := false
			select {
			case result = <-done:
				prompt = true
			case <-time.After(time.Second):
			}
			// Release and join before reporting an accidental gate wait.
			m.gate.Unlock()
			if !prompt {
				cancel()
				select {
				case result = <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("diagnostic did not join after gate release/cancellation")
				}
			}
			requireBusyStorageDiagnostic(t, prompt, result.value, result.err, m)
		})
	}
}

func TestMemberStorageIndependentClosureDebtIsReadOnly(t *testing.T) {
	f := capacityFixture(t, 4)
	accept(t, f.s, newID(t), f.envelope(t), "diagnostics must not reconcile this original")
	m := f.members[0]
	requireReceiptNoError(
		t,
		os.WriteFile(
			filepath.Join(m.root, "stage-diagnostic-sentinel"),
			[]byte("no filesystem recovery from diagnostics"),
			0o600,
		),
	)
	beforeFiles := memberFilesTree(t, m.root)
	beforeAuthority := storageDiagnosticAuthority(t, f, m.id)
	// Inject only an existing conservative flag. No actual close failure or
	// permission to clear real closure debt is claimed by this diagnostic test.
	m.readClosureUncertain.Store(true)
	defer m.readClosureUncertain.Store(false)
	for range 2 {
		result, err := f.s.MemberStorage(context.Background(), m.id, ID{}, 1)
		requireReceiptNoError(t, err)
		runtime := result.Runtime
		requireReceiptCondition(t, runtime.Status == "sampled", "diagnostic erased or concealed closure debt", runtime)
		requireReceiptCondition(t, runtime.Frozen != nil, "diagnostic erased or concealed closure debt", runtime)
		requireReceiptCondition(t, *runtime.Frozen, "diagnostic erased or concealed closure debt", runtime)
		requireReceiptCondition(
			t,
			runtime.ClosureUncertain != nil,
			"diagnostic erased or concealed closure debt",
			runtime,
		)
		requireReceiptCondition(t, *runtime.ClosureUncertain, "diagnostic erased or concealed closure debt", runtime)
		requireReceiptCondition(t, runtime.Closed != nil, "diagnostic erased or concealed closure debt", runtime)
		requireReceiptCondition(t, !*runtime.Closed, "diagnostic erased or concealed closure debt", runtime)
		requireReceiptCondition(t, runtime.CurrentFile != nil, "sampled runtime lacks separately observed identity")
		requireReceiptCondition(
			t,
			*runtime.CurrentFile == m.bytes.FileID().String(),
			"sampled runtime lacks separately observed identity",
		)
		requireReceiptCondition(t, runtime.SampledAt != nil, "sampled runtime lacks separately observed identity")
	}
	requireReceiptCondition(t, m.readClosureUncertain.Load(), "diagnostics cleared member closure debt")
	requireReceiptCondition(t, m.isFrozen(), "diagnostics cleared member closure debt")
	requireMemberRootOwnership(t, m)
	memberFilesUnchanged(t, m.root, beforeFiles)
	requireReceiptCondition(
		t,
		storageDiagnosticAuthority(t, f, m.id) == beforeAuthority,
		"diagnostic read mutated storage authority",
	)
}

func TestMemberStorageIndependentNewOwnerCannotRefreshDescriptor(t *testing.T) {
	f := capacityFixture(t, 4)
	var identities atomic.Int64
	countIdentity := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/identity" {
				identities.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	}
	store, members, servers := remoteMaintenance(t, f, countIdentity)
	remote, oldServer, oldMember := members[0], servers[0], f.members[0]
	bound := remote.identity()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	next, server := replaceStorageDiagnosticOwner(t, ctx, f, oldServer, oldMember, countIdentity)
	serveStorageDiagnosticReplacement(t, ctx, t.Context(), remote, server)
	requireReceiptCondition(t, server.owner.epoch != bound.OwnerEpoch, "fixture did not create a new owner generation")
	beforeIdentityRequests := identities.Load()
	before := storageDiagnosticAuthority(t, f, next.id)
	result, err := readStorageWithoutIdentityRefresh(t, ctx, cancel, store, remote, next.id)
	requireReceiptCondition(t, err == nil, "new authenticated owner was not reported separately", err, result.Runtime)
	requireReceiptCondition(
		t,
		result.Runtime.Status == "generation_mismatch",
		"new authenticated owner was not reported separately",
		err,
		result.Runtime,
	)
	requireStorageDiagnosticOwnerEpochs(t, result, server.owner.epoch, bound.OwnerEpoch)
	requireReceiptCondition(t, remote.identity() == bound, "diagnostic adopted an owner or called identity refresh")
	requireReceiptCondition(
		t,
		identities.Load() == beforeIdentityRequests,
		"diagnostic adopted an owner or called identity refresh",
	)
	requireReceiptCondition(
		t,
		storageDiagnosticAuthority(t, f, next.id) == before,
		"generation mismatch mutated durable authority",
	)
}

func TestMemberStorageIndependentMixedHistoryPagination(t *testing.T) {
	f := rotationFixture(t)
	ctx := context.Background()
	m := f.members[0]
	dead := accept(t, f.s, newID(t), f.envelope(t), "dead diagnostic history")
	accept(t, f.s, newID(t), f.envelope(t), "retained diagnostic history")
	requireReceiptNoError(t, f.s.RotateHot(ctx, m.id))
	expireReceipt(t, f, dead)
	requireReceiptNoError(t, f.s.ReclaimSealed(ctx, m.id))
	accept(t, f.s, newID(t), f.envelope(t), "second retained sealed file")
	requireReceiptNoError(t, f.s.RotateHot(ctx, m.id))
	before := storageDiagnosticAuthority(t, f, m.id)
	beforeFiles := memberFilesTree(t, m.root)
	states, seen := inspectStorageHistoryPages(t, ctx, f, m)
	requireReceiptCondition(t, len(seen) == 4, "mixed retained/removed file history was lost", states)
	requireReceiptCondition(t, states["current"] == 1, "mixed retained/removed file history was lost", states)
	requireReceiptCondition(t, states["sealed"] == 2, "mixed retained/removed file history was lost", states)
	requireReceiptCondition(t, states["removed"] == 1, "mixed retained/removed file history was lost", states)
	requireReceiptCondition(
		t,
		storageDiagnosticAuthority(t, f, m.id) == before,
		"paginated inspection mutated authority",
	)
	memberFilesUnchanged(t, m.root, beforeFiles)
}

func TestMemberStorageIndependentFileSeekUsesBoundedIndex(t *testing.T) {
	f := capacityFixture(t, 4)
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer rollback(context.Background(), tx)
	// Planner-only fixture: clone catalog constraints/indexes into a temporary
	// table. These rows claim no receipt, disk file, or real storage authority.
	_, err = tx.Exec(
		ctx,
		`CREATE TEMP TABLE member_storage_plan_files (LIKE fry_member_file INCLUDING DEFAULTS `+
			`INCLUDING CONSTRAINTS INCLUDING INDEXES) ON COMMIT DROP`,
	)
	requireReceiptNoError(t, err)
	_, err = tx.Exec(
		ctx,
		`INSERT INTO `+
			`member_storage_plan_files(member,file_id,directory,state,rewrite_job,charged_bytes) `+
			`SELECT CASE WHEN n%2=0 THEN $1 ELSE $3 `+
			`END,lpad(to_hex(n),32,'0'),'generation-'||lpad(to_hex(n),32,'0'),'removed',$2,0 FROM `+
			`generate_series(1,4096)n`,
		f.members[0].id.String(),
		newID(t).String(),
		f.members[1].id.String(),
	)
	requireReceiptNoError(t, err)
	_, err = tx.Exec(ctx, `ANALYZE member_storage_plan_files`)
	requireReceiptNoError(t, err)
	query := strings.ReplaceAll(memberStorageFilesQuery, "fry_member_file", "member_storage_plan_files")
	members := []string{f.members[0].id.String(), f.members[1].id.String()}
	if members[0] > members[1] {
		members[0], members[1] = members[1], members[0]
	}
	for order, member := range members {
		for _, cursor := range []struct {
			name  string
			after string
		}{
			{"initial", ""},
			{"middle", fmt.Sprintf("%032x", 2048)},
			{"tail", fmt.Sprintf("%032x", 4090)},
			{"end", fmt.Sprintf("%032x", 4096)},
			{"beyond", fmt.Sprintf("%032x", 5000)},
		} {
			t.Run(fmt.Sprintf("member_order=%d/%s", order, cursor.name), func(t *testing.T) {
				requireStoragePlanSeek(t, ctx, f, tx, query, member, cursor.after)
			})
		}
	}
}

func replaceStorageDiagnosticOwner(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	oldServer *MemberServer,
	oldMember *LocalMember,
	countIdentity func(http.Handler) http.Handler,
) (*LocalMember, *MemberServer) {
	t.Helper()
	requireReceiptNoError(t, oldServer.Close(ctx))
	next, err := OpenLocalMember(
		context.WithoutCancel(ctx),
		f.pool,
		oldMember.id,
		oldMember.domain,
		oldMember.root,
		oldMember.opts,
	)
	requireReceiptNoError(t, err)
	var keys [][32]byte
	for key := range oldServer.keys {
		keys = append(keys, key)
	}
	server, err := NewMemberServer(
		ctx,
		next,
		MemberServerOptions{TLS: oldServer.tls, CoordinatorKeys: keys, MaxRequests: 8, Timeout: 5 * time.Second},
	)
	if err != nil {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal(err)
	}
	server.http.Handler = countIdentity(server.http.Handler)
	return next, server
}

func serveStorageDiagnosticReplacement(
	t *testing.T,
	ctx, listenContext context.Context,
	remote *RemoteMember,
	server *MemberServer,
) {
	t.Helper()
	endpoint, err := url.Parse(remote.endpoint)
	if err != nil {
		if err := server.Close(ctx); err != nil {
			t.Error(err)
		}
		t.Fatal(err)
	}
	listener, err := (&net.ListenConfig{}).Listen(listenContext, "tcp", endpoint.Host)
	if err != nil {
		if err := server.Close(ctx); err != nil {
			t.Error(err)
		}
		t.Fatal(err)
	}
	serving := make(chan error, 1)
	go func() { serving <- server.Serve(listener) }()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if err := server.Close(cleanup); err != nil {
			t.Error(err)
		}
		select {
		case err := <-serving:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Error(err)
			}
		case <-cleanup.Done():
			t.Error("replacement diagnostic server did not stop")
		}
	})
}

func readStorageWithoutIdentityRefresh(
	t *testing.T,
	ctx context.Context,
	cancel context.CancelFunc,
	store *Store,
	remote *RemoteMember,
	member ID,
) (MemberStorageSnapshot, error) {
	t.Helper()
	// Holding the refresh mutex separately proves diagnostics do not acquire it.
	type outcome struct {
		value MemberStorageSnapshot
		err   error
	}
	done := make(chan outcome, 1)
	remote.refreshMu.Lock()
	go func() {
		value, err := store.MemberStorage(ctx, member, ID{}, 1)
		done <- outcome{value, err}
	}()
	var observed outcome
	prompt := false
	select {
	case observed = <-done:
		prompt = true
	case <-time.After(time.Second):
	}
	remote.refreshMu.Unlock()
	if !prompt {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("diagnostic did not join after refresh mutex release")
		}
		t.Fatal("diagnostic waited on identity refresh mutex")
	}
	result, err := observed.value, observed.err
	return result, err
}

func inspectStorageHistoryPages(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	m *LocalMember,
) (map[string]int, map[string]bool) {
	t.Helper()
	states := map[string]int{}
	seen := map[string]bool{}
	var after ID
	last := ""
	for page := 0; ; page++ {
		requireReceiptCondition(t, page < 8, "pagination did not terminate within the known four-file fixture")
		result, err := f.s.MemberStorage(ctx, m.id, after, 1)
		requireReceiptNoError(t, err)
		requireReceiptCondition(
			t,
			len(result.Files) == 1,
			"page exceeded limit or skipped expected history",
			result.Files,
		)
		file := result.Files[0]
		requireReceiptCondition(t, !seen[file.FileID], "keyset pagination repeated or reordered history", file)
		requireReceiptCondition(t, file.FileID > last, "keyset pagination repeated or reordered history", file)
		seen[file.FileID], last = true, file.FileID
		states[file.State]++
		requireStorageHistoryCurrentSnapshot(t, result, m)
		requireReceiptCondition(
			t,
			file.State != "removed" || file.ChargedBytes == "0",
			"removed history was charged as active bytes",
		)
		if result.NextFile == nil {
			break
		}
		requireReceiptCondition(t, *result.NextFile == file.FileID, "next cursor skipped a nonreturned file")
		after, err = idFrom(*result.NextFile)
		requireReceiptNoError(t, err)
	}
	return states, seen
}

func requireStoragePlanSeek(t *testing.T, ctx context.Context, f *fixture, tx pgx.Tx, query, member, after string) {
	t.Helper()
	want := expectedStoragePlanFiles(t, f, member, after)
	got := readStoragePlanFiles(t, ctx, tx, query, member, after)
	requireReceiptCondition(
		t,
		slices.Equal(got, want),
		"seek returned another member, skipped or reordered files",
		got,
		want,
	)
	var raw []byte
	requireReceiptNoError(t, tx.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+query, member, after, 65).Scan(&raw))
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	{
		err := json.Unmarshal(raw, &plans)
		requireReceiptCondition(t, err == nil, "invalid explain result", err)
		requireReceiptCondition(t, len(plans) == 1, "invalid explain result", err)
	}
	indexes := requireBoundedStoragePlan(t, plans[0].Plan, member, after, raw)
	requireReceiptCondition(
		t,
		indexes == 1,
		"expected one indexed bounded lookahead",
		indexes,
		plans[0].Plan["Actual Rows"],
		len(want),
	)
	requireReceiptCondition(
		t,
		plans[0].Plan["Actual Rows"] == float64(len(want)),
		"expected one indexed bounded lookahead",
		indexes,
		plans[0].Plan["Actual Rows"],
		len(want),
	)
}

func expectedStoragePlanFiles(t *testing.T, f *fixture, member, after string) []string {
	t.Helper()
	var want []string
	for n := 1; n <= 4096 && len(want) < 65; n++ {
		file := fmt.Sprintf("%032x", n)
		belongs := (n%2 == 0) == (member == f.members[0].id.String())
		if belongs && file > after {
			want = append(want, file)
		}
	}
	return want
}

func readStoragePlanFiles(t *testing.T, ctx context.Context, tx pgx.Tx, query, member, after string) []string {
	t.Helper()
	rows, err := tx.Query(ctx, query, member, after, 65)
	requireReceiptNoError(t, err)
	var got []string
	for rows.Next() {
		var file, state string
		var charge int64
		var sealed *int64
		var rewrite *string
		if err := rows.Scan(&file, &state, &charge, &sealed, &rewrite); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		got = append(got, file)
	}
	rows.Close()
	requireReceiptNoError(t, rows.Err())
	return got
}

func requireBoundedStoragePlan(t *testing.T, node map[string]any, member, after string, raw []byte) int {
	t.Helper()
	indexes := 0

	typeName := receiptTestValue[string](t, node["Node Type"])
	requireReceiptConditionf(
		t,
		typeName != "Sort",
		"bounded file seek scanned/sorted catalog prefix: %s\n%s",
		typeName,
		raw,
	)
	requireReceiptConditionf(
		t,
		typeName != "Seq Scan",
		"bounded file seek scanned/sorted catalog prefix: %s\n%s",
		typeName,
		raw,
	)
	requireReceiptConditionf(
		t,
		typeName != "Bitmap Heap Scan",
		"bounded file seek scanned/sorted catalog prefix: %s\n%s",
		typeName,
		raw,
	)
	if typeName == "Index Scan" || typeName == "Index Only Scan" {
		indexes++
		condition := receiptTestValue[string](t, node["Index Cond"])
		rows := receiptTestValue[float64](t, node["Actual Rows"])
		filtered := receiptTestValue[float64](t, node["Rows Removed by Filter"])
		memberUpperBound := strings.Contains(condition, fmt.Sprintf("member <= '%s'::text", member))
		cursorLowerBound := strings.Contains(
			condition,
			fmt.Sprintf("ROW(member, file_id) > ROW('%s'::text, '%s'::text)", member, after),
		)
		requireReceiptConditionf(
			t,
			memberUpperBound && cursorLowerBound && rows <= 65 && filtered == 0,
			"index did not bound both member and cursor: condition=%q rows=%v filtered=%v\n%s",
			condition,
			rows,
			filtered,
			raw,
		)
	}
	children := receiptTestValue[[]any](t, node["Plans"])
	for _, child := range children {
		indexes += requireBoundedStoragePlan(t, receiptTestValue[map[string]any](t, child), member, after, raw)
	}
	return indexes
}

func requireBusyStorageDiagnostic(t *testing.T, prompt bool, result MemberStorageSnapshot, err error, m *LocalMember) {
	t.Helper()
	requireReceiptCondition(
		t,
		prompt,
		"diagnostic waited for member gate or advertised sampled state",
		prompt,
		err,
		result.Runtime,
	)
	requireReceiptCondition(
		t,
		err == nil,
		"diagnostic waited for member gate or advertised sampled state",
		prompt,
		err,
		result.Runtime,
	)
	requireReceiptCondition(
		t,
		result.Runtime.Status == "gate_busy",
		"diagnostic waited for member gate or advertised sampled state",
		prompt,
		err,
		result.Runtime,
	)
	requireStorageRuntimeUnknown(t, result.Runtime)
	requireReceiptCondition(
		t,
		!result.SQLSnapshotAt.IsZero(),
		"busy runtime discarded independent metadata snapshot",
	)
	requireReceiptCondition(
		t,
		!result.Runtime.CheckedAt.IsZero(),
		"busy runtime discarded independent metadata snapshot",
	)
	requireReceiptCondition(
		t,
		result.Current.FileID == m.bytes.FileID().String(),
		"busy runtime discarded independent metadata snapshot",
	)
}

func requireStorageDiagnosticOwnerEpochs(t *testing.T, result MemberStorageSnapshot, observedEpoch, boundEpoch int64) {
	t.Helper()
	requireStorageRuntimeUnknown(t, result.Runtime)
	requireReceiptCondition(
		t,
		result.Runtime.OwnerEpoch != nil,
		"runtime confused observed and bound generations",
		result.Runtime,
	)
	requireReceiptCondition(
		t,
		*result.Runtime.OwnerEpoch == strconv.FormatInt(observedEpoch, 10),
		"runtime confused observed and bound generations",
		result.Runtime,
	)
	requireReceiptCondition(
		t,
		result.Runtime.BoundOwnerEpoch != nil,
		"runtime confused observed and bound generations",
		result.Runtime,
	)
	requireReceiptCondition(
		t,
		*result.Runtime.BoundOwnerEpoch == strconv.FormatInt(boundEpoch, 10),
		"runtime confused observed and bound generations",
		result.Runtime,
	)
}

func requireStorageHistoryCurrentSnapshot(t *testing.T, result MemberStorageSnapshot, m *LocalMember) {
	t.Helper()
	requireReceiptCondition(
		t,
		result.Current.FileID == m.bytes.FileID().String(),
		"current snapshot confused historical files with empty current",
		result.Current,
	)
	requireReceiptCondition(
		t,
		len(result.Current.Lanes) <= 16,
		"current snapshot confused historical files with empty current",
		result.Current,
	)
	requireReceiptCondition(
		t,
		result.Current.ReservedRecords != nil,
		"current snapshot confused historical files with empty current",
		result.Current,
	)
	requireReceiptCondition(
		t,
		*result.Current.ReservedRecords == "0",
		"current snapshot confused historical files with empty current",
		result.Current,
	)
}
