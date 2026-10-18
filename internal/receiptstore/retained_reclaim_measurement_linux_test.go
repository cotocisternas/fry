//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
)

func retainedReclaimSetting(t *testing.T, name string, fallback, minimum, maximum int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	requireReceiptConditionf(t, err == nil, "invalid bounded %s", name)
	requireReceiptConditionf(t, n >= minimum, "invalid bounded %s", name)
	requireReceiptConditionf(t, n <= maximum, "invalid bounded %s", name)
	return n
}

// Opt-in real acceptance/reclamation, not a production benchmark. Setup and all
// verification run outside the first-member ReclaimHot measurement window.
func TestRetainedReclaimMeasurement(t *testing.T) {
	if os.Getenv("FRY_RETAINED_RECLAIM_MEASUREMENT") != "1" {
		t.Skip("opt-in retained hot reclamation measurement")
	}
	count := retainedReclaimSetting(t, "FRY_RECLAIM_RECORDS", 1024, 128, 4096)
	size := retainedReclaimSetting(t, "FRY_RECLAIM_MESSAGE_BYTES", 1024, 128, 65536)
	requireReceiptCondition(t, count%2 == 0, "even record count and <=256MiB input required")
	requireReceiptCondition(t, int64(count)*int64(size) <= 256<<20, "even record count and <=256MiB input required")
	var report map[string]any
	requireReceiptCondition(t, t.Run("real_accepted_fixture", func(t *testing.T) {
		report = runRetainedReclamation(t, count, size)
	}), "reclamation fixture or checked cleanup failed")
	raw, err := json.Marshal(report)
	requireReceiptNoError(t, err)
	if _, err := fmt.Printf("RETAINED_RECLAIM_MEASUREMENT_JSON %s\n", raw); err != nil {
		t.Error(err)
	}
}

func runRetainedReclamation(t *testing.T, count, size int) map[string]any {
	t.Helper()
	hot := hotstore.Options{
		MaxMessageBytes:      int64(size),
		MaxBytes:             int64(count)*int64(size)*4 + (16 << 20),
		MaxConcurrentAppends: 1,
	}
	f := setupWithPoolConfig(t, nil, hot, Options{Lease: 30 * time.Second, RecoveryRetention: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	envelope, dead, live := acceptRetainedReclaimPopulation(t, ctx, f, count, size)
	expireRetainedReclaimPopulation(t, ctx, f, envelope, dead)
	m := f.members[0]
	oldFile, oldDirectory := m.bytes.FileID(), m.directory
	oldPath := filepath.Join(m.root, oldDirectory, "data.fry")
	beforeFile, err := os.Stat(oldPath)
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, len(m.bytes.Inventory()) == count, "unexpected input inventory")
	elapsed, before, after, postGC := measureRetainedHotReclaim(t, ctx, f, m)
	newFile, locators := verifyRetainedReclaimMember(t, ctx, m, oldFile, oldDirectory, oldPath, live, dead, size)
	verifyRetainedReclaimTargets(t, ctx, f, m, dead, locators, count)
	finished := finishRetainedReclamation(t, ctx, f, m, live, dead, size)
	version, hashes, fsType := retainedReclaimEnvironment(t, ctx, f)
	for _, member := range f.members {
		err = member.Close()
		requireReceiptCondition(t, err == nil, "checked member close failed", err)
	}
	return map[string]any{
		"profile":                         "real-accepted-retained-hot-reclaim",
		"records":                         count,
		"message_bytes":                   size,
		"expired_records":                 len(dead),
		"live_records":                    len(live),
		"live_original_bytes":             int64(len(live)) * int64(size),
		"expired_original_bytes":          int64(len(dead)) * int64(size),
		"first_member_reclaim_elapsed_ms": float64(elapsed) / float64(time.Millisecond),
		"allocated_bytes_during_reclaim":  after.TotalAlloc - before.TotalAlloc,
		"gc_cycles_during_reclaim":        after.NumGC - before.NumGC,
		"heap_alloc_before_after_forced_gc": map[string]uint64{
			"before": before.HeapAlloc,
			"after":  postGC.HeapAlloc,
		},
		"first_member_file_bytes_before":             beforeFile.Size(),
		"first_member_file_bytes_after":              newFile.Size(),
		"verified_live_direct_from_rewritten_member": len(live),
		"finished_removed_receipts":                  finished,
		"go":                                         runtime.Version(),
		"os":                                         runtime.GOOS,
		"arch":                                       runtime.GOARCH,
		"logical_cpus":                               runtime.NumCPU(),
		"gomaxprocs":                                 runtime.GOMAXPROCS(0),
		"postgresql_version":                         version,
		"source_sha256":                              hashes,
		"filesystem_type":                            fsType,
		"gogc_environment":                           os.Getenv("GOGC"),
		"gomemlimit_environment":                     os.Getenv("GOMEMLIMIT"),
		"member_close_checked":                       true,
		"scope": "Go test process memory; PostgreSQL excluded; first whole ReclaimHot call wall time is " +
			"not exact exclusive gate wait; setup/verification/second member/FinishCleanup " +
			"excluded; warm filesystem on shared unisolated local machine; not production capacity",
	}
}

func acceptRetainedReclaimPopulation(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	count, size int,
) (Envelope, map[ID]bool, map[ID]Receipt) {
	t.Helper()
	payload := bytes.Repeat([]byte("x"), size)
	copy(payload, []byte("Subject: retained reclaim\r\n\r\n"))
	envelope := f.envelope(t)
	dead := map[ID]bool{}
	live := map[ID]Receipt{}
	for i := range count {
		copy(payload[len(payload)-16:], []byte(fmt.Sprintf("%016x", i)))
		expected := sha256.Sum256(payload)
		r, err := f.s.Accept(ctx, newID(t), envelope, io.NopCloser(bytes.NewReader(payload)))
		requireReceiptCondition(t, err == nil, "real acceptance failed", err)
		requireReceiptCondition(t, r.SHA256 == expected, "accepted identity differs from independent original hash")
		requireReceiptCondition(t, r.Size == int64(size), "accepted identity differs from independent original hash")
		if i%2 == 0 {
			dead[r.ID] = true
		} else {
			live[r.ID] = r
		}
	}
	return envelope, dead, live
}

func expireRetainedReclaimPopulation(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	envelope Envelope,
	dead map[ID]bool,
) {
	t.Helper()
	for id := range dead {
		requireReceiptNoError(
			t,
			f.s.ReleaseReference(
				ctx,
				f.org,
				id,
				Reference{Kind: "pending-mailbox", Key: envelope.Destinations[0].Inbox.String()},
			),
		)
	}
	// Let the final receipt's one-second recovery deadline expire naturally.
	timer := time.NewTimer(1100 * time.Millisecond)
	select {
	case <-ctx.Done():
		timer.Stop()
		t.Fatal(ctx.Err())
	case <-timer.C:
	}
	for {
		n, err := f.s.SweepExpiry(ctx, 1024)
		requireReceiptNoError(t, err)
		if n == 0 {
			break
		}
	}
	var retiring int
	{
		err := f.pool.QueryRow(ctx, `SELECT count(*) FROM fry_receipt WHERE lifecycle='retiring'`).Scan(&retiring)
		requireReceiptCondition(t, err == nil, "wrong naturally expired population", retiring, err)
		requireReceiptCondition(t, retiring == len(dead), "wrong naturally expired population", retiring, err)
	}
}

func verifyRetainedReclaimMember(
	t *testing.T,
	ctx context.Context,
	m *LocalMember,
	oldFile ID,
	oldDirectory, oldPath string,
	live map[ID]Receipt,
	dead map[ID]bool,
	size int,
) (os.FileInfo, map[ID]hotstore.Locator) {
	t.Helper()
	var err error
	requireReceiptCondition(t, oldFile != m.bytes.FileID(), "file incarnation unchanged")
	{
		_, err = os.Stat(oldPath)
		requireReceiptCondition(t, errors.Is(err, os.ErrNotExist), "old generation bytes remain", err)
	}
	if oldDirectory != "." {
		{
			_, err = os.Stat(filepath.Join(m.root, oldDirectory))
			requireReceiptCondition(t, errors.Is(err, os.ErrNotExist), "old generation directory remains", err)
		}
	}
	newFile, err := os.Stat(filepath.Join(m.root, m.directory, "data.fry"))
	requireReceiptNoError(t, err)
	inventory := m.bytes.Inventory()
	requireReceiptCondition(t, len(inventory) == len(live), "dead physical records retained")
	locators := map[ID]hotstore.Locator{}
	for _, loc := range inventory {
		r, ok := live[loc.Key.Receipt]
		requireReceiptCondition(t, ok, "unexpected rewritten locator")
		requireReceiptCondition(t, !dead[loc.Key.Receipt], "unexpected rewritten locator")
		requireReceiptCondition(t, loc.Key.Attempt == r.Attempt, "unexpected rewritten locator")
		requireReceiptCondition(t, loc.File == m.bytes.FileID(), "unexpected rewritten locator")
		digest := sha256.New()
		{
			err = m.bytes.Read(ctx, loc, 0, r.Size, digest)
			requireReceiptCondition(t, err == nil, "direct rewritten member read failed", err)
		}
		requireReceiptCondition(t, bytes.Equal(digest.Sum(nil), r.SHA256[:]), "direct rewritten member bytes differ")
		requireReceiptCondition(t, r.Size == int64(size), "direct rewritten member bytes differ")
		locators[r.ID] = loc
	}
	return newFile, locators
}

func verifyRetainedReclaimTargets(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	m *LocalMember,
	dead map[ID]bool,
	locators map[ID]hotstore.Locator,
	count int,
) {
	t.Helper()
	rows, err := f.pool.Query(
		ctx,
		`SELECT r.id,t.locator,t.bytes_removed FROM fry_receipt r JOIN fry_target t ON `+
			`t.attempt=r.attempt WHERE t.member=$1`,
		m.id.String(),
	)
	requireReceiptNoError(t, err)
	defer rows.Close()
	targets := 0
	for rows.Next() {
		var rawID string
		var raw []byte
		var removed bool
		if err = rows.Scan(&rawID, &raw, &removed); err != nil {
			break
		}
		id, e := idFrom(rawID)
		if e != nil {
			err = e
			break
		}
		var loc hotstore.Locator
		if e = json.Unmarshal(raw, &loc); e != nil {
			err = e
			break
		}
		if dead[id] {
			requireReceiptCondition(t, removed, "dead target not removed")
		} else {
			requireReceiptCondition(t, !removed, "database live locator mismatch")
			requireReceiptCondition(t, loc == locators[id], "database live locator mismatch")
		}
		targets++
	}
	rowErr := rows.Err()
	rows.Close()
	requireReceiptCondition(t, err == nil, "target inspection", err, rowErr, targets)
	requireReceiptCondition(t, rowErr == nil, "target inspection", err, rowErr, targets)
	requireReceiptCondition(t, targets == count, "target inspection", err, rowErr, targets)
}

func finishRetainedReclamation(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	m *LocalMember,
	live map[ID]Receipt,
	dead map[ID]bool,
	size int,
) int {
	t.Helper()
	var err error
	var dbIncarnation string
	{
		err = f.pool.QueryRow(ctx, `SELECT incarnation FROM fry_member WHERE id=$1`, m.id.String()).Scan(&dbIncarnation)
		requireReceiptCondition(t, err == nil, "published member incarnation mismatch", err)
		requireReceiptCondition(
			t,
			dbIncarnation == m.bytes.FileID().String(),
			"published member incarnation mismatch",
			err,
		)
	}
	err = f.s.ReclaimHot(ctx, f.members[1].id)
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, len(f.members[1].bytes.Inventory()) == len(live), "second member dead records remain")
	finished := 0
	for {
		n, e := f.s.FinishCleanup(ctx, 1024)
		requireReceiptNoError(t, e)
		finished += n
		if n == 0 {
			break
		}
	}
	requireReceiptCondition(t, finished == len(dead), "cleanup count", finished)
	var removed int
	{
		err = f.pool.QueryRow(ctx, `SELECT count(*) FROM fry_receipt WHERE lifecycle='removed'`).Scan(&removed)
		requireReceiptCondition(t, err == nil, "removed lifecycle count", removed, err)
		requireReceiptCondition(t, removed == len(dead), "removed lifecycle count", removed, err)
	}
	usage, err := f.s.OrganizationStorageUsage(ctx, f.org)
	requireReceiptCondition(t, err == nil, "retained accounting mismatch", usage, err)
	requireReceiptCondition(t, usage.RetainedOriginals == int64(len(live)), "retained accounting mismatch", usage, err)
	requireReceiptCondition(t, usage.RetainedBytes == int64(len(live)*size), "retained accounting mismatch", usage, err)
	return finished
}

func retainedReclaimEnvironment(t *testing.T, ctx context.Context, f *fixture) (string, map[string]string, string) {
	t.Helper()
	var err error
	var version string
	err = f.pool.QueryRow(ctx, `SELECT version()`).Scan(&version)
	requireReceiptNoError(t, err)
	hashes := map[string]string{}
	for _, name := range []string{
		"retained_reclaim_measurement_linux_test.go",
		"hot_cleanup_linux.go",
		"lifecycle_linux.go",
		"store_linux.go",
		"postgres.go",
		"storage_usage_linux.go",
		"../../db/schema/receiptstore.sql",
		"../hotstore/store_linux.go",
		"../hotstore/image_linux.go",
		"../hotstore/recovery_linux.go",
		"../hotstore/format.go",
		"receiptstore_linux_test.go",
		"../../go.mod",
		"../../go.sum",
		"../../scripts/bench.sh",
		"../../scripts/bench/retained_reclaim.py",
		"../../scripts/lib/benchmark.py",
		"../../scripts/lib/testdeps.py",
	} {
		raw, e := storageReadFile("../..", filepath.Join("internal/receiptstore", name))
		requireReceiptCondition(t, e == nil, "source identity unavailable")
		h := sha256.Sum256(raw)
		hashes[name] = hex.EncodeToString(h[:])
	}
	var filesystem syscall.Statfs_t
	err = syscall.Statfs(f.dirs[0], &filesystem)
	requireReceiptNoError(t, err)
	fsType := fmt.Sprintf("0x%x", filesystem.Type)
	if filesystem.Type == 0x9123683e {
		fsType = "btrfs"
	}
	if filesystem.Type == 0x01021994 {
		fsType = "tmpfs"
	}
	return version, hashes, fsType
}

func measureRetainedHotReclaim(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	m *LocalMember,
) (time.Duration, runtime.MemStats, runtime.MemStats, runtime.MemStats) {
	t.Helper()
	runtime.GC()
	var before, after, postGC runtime.MemStats
	runtime.ReadMemStats(&before)
	began := time.Now()
	err := f.s.ReclaimHot(ctx, m.id)
	elapsed := time.Since(began)
	runtime.ReadMemStats(&after)
	requireReceiptCondition(t, err == nil, "first actual reclamation failed", err)
	runtime.GC()
	runtime.ReadMemStats(&postGC)
	return elapsed, before, after, postGC
}
