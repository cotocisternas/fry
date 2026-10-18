//go:build linux

package hotstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This opt-in test measures retained byte-store records, not accepted FRY mail.
// Durable staging and complete independent verification are outside phase clocks.
func retainedWorkload() (int, int, error) {
	count, err := strconv.Atoi(os.Getenv("FRY_HOTSTORE_RETAINED_RECORDS"))
	if err != nil {
		return 0, 0, err
	}
	size, err := strconv.Atoi(os.Getenv("FRY_HOTSTORE_RETAINED_BYTES"))
	if err != nil {
		return 0, 0, err
	}
	if (size != 1024 || (count != 1024 && count != 16384 && count != 65536)) && (size != 65536 || count != 1024) {
		return 0, 0, fmt.Errorf("unsupported fixed retained workload")
	}
	return count, size, nil
}

func retainedOptions(count, size int) Options {
	return Options{
		MaxMessageBytes:      int64(size),
		MaxBytes:             fileHeaderSize + int64(count+2)*encodedSize(int64(size)),
		MaxConcurrentAppends: 1,
	}
}

func retainedKey(i int) Key {
	if i < 0 || i >= 65536 {
		panic("invalid retained key index")
	}
	var key Key
	key.Receipt[0] = 0x11
	key.Attempt[0] = 0x22
	binary.BigEndian.PutUint64(key.Receipt[8:], uint64(i+1))
	binary.BigEndian.PutUint64(key.Attempt[8:], uint64(i+1))
	return key
}

func retainedPayload(buf []byte, i int) {
	for j := range buf {
		buf[j] = 'x'
	}
	copy(buf, fmt.Sprintf("Synthetic hotstore retained original %08d\r\n", i))
	copy(buf[len(buf)-2:], "\r\n")
}

func retainedRSS() (map[string]int64, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil, err
	}
	result := map[string]int64{}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && (fields[0] == "VmRSS:" || fields[0] == "VmHWM:") {
			n, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return nil, err
			}
			if fields[2] != "kB" {
				return nil, fmt.Errorf("unexpected proc RSS unit")
			}
			result[strings.TrimSuffix(fields[0], ":")] = n * 1024
		}
	}
	if len(result) != 2 {
		return nil, fmt.Errorf("RSS/HWM missing")
	}
	return result, nil
}

func retainedWriteReport(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	requireHotNoError(t, err)
	// Report path is parent-selected in an opted-in run; O_EXCL prevents replacing an existing file; owner: FRY
	// maintainers; expires: 2027-01-18.
	// #nosec G304 G703 -- See the path-safety rationale above.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	requireHotNoError(t, err)
	_, writeErr := file.Write(append(raw, '\n'))
	syncErr := file.Sync()
	closeErr := file.Close()
	requireHotCondition(
		t,
		((writeErr == nil && syncErr == nil) && closeErr == nil),
		"report write failed",
		writeErr,
		syncErr,
		closeErr,
	)
}

func retainedVerify(
	t *testing.T,
	store *Store,
	inventory []Locator,
	count, size int,
	expectedID string,
	expectedFileBytes int64,
) {
	t.Helper()
	if count < 0 || count > 65536 {
		t.Fatal("invalid retained record count")
		return
	}
	requireHotCondition(
		t,
		(store.FileID().String() == expectedID && len(inventory) == count),
		"retained identity/count mismatch",
	)
	info, err := store.file.Stat()
	requireHotCondition(t, (err == nil && info.Size() == expectedFileBytes), "retained file length mismatch", err)
	verifyRetainedBodies(t, store, inventory, count, size)
}

func verifyRetainedBodies(t *testing.T, store *Store, inventory []Locator, count, size int) {
	t.Helper()
	seen := make([]bool, count)
	payload := make([]byte, size)
	for _, loc := range inventory {
		i := retainedLocatorIndex(t, loc, count)
		requireHotCondition(
			t,
			(((!seen[i] && loc.Key == retainedKey(i)) && loc.File == store.FileID()) && loc.Size == int64(size)),
			"duplicate/changed retained locator",
		)
		seen[i] = true
		retainedPayload(payload, i)
		expected := sha256.Sum256(payload)
		hash := sha256.New()
		{
			err := store.Read(context.Background(), loc, 0, loc.Size, hash)
			requireHotCondition(t, err == nil, "retained original read", err)
		}
		requireHotCondition(
			t,
			(loc.SHA256 == expected && bytes.Equal(hash.Sum(nil), expected[:])),
			"retained original hash mismatch",
		)
	}
	for _, found := range seen {
		requireHotCondition(t, found, "missing retained original")
	}
}

func TestHotstoreRetainedMeasurementChild(t *testing.T) {
	phase, count, size, root, output, expectedSize := retainedChildConfig(t)
	var err error
	var store *Store
	if phase == "inventory" {
		store, err = Open(root, retainedOptions(count, size))
		requireHotNoError(t, err)
	}
	rssBefore, err := retainedRSS()
	requireHotNoError(t, err)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	var inventory []Locator
	if phase == "open" {
		store, err = Open(root, retainedOptions(count, size))
	} else {
		inventory = store.Inventory()
	}
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	rssAfter, memErr := retainedRSS()
	requireHotCondition(t, (err == nil && memErr == nil), "phase failed", err, memErr)
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	retained, heapDelta := retainedHeapAfterGC(t, before, store, inventory)
	report := map[string]any{
		"phase":                                   phase,
		"records":                                 count,
		"message_bytes":                           size,
		"raw_bytes":                               int64(count) * int64(size),
		"encoded_file_bytes":                      expectedSize,
		"elapsed_ms":                              float64(elapsed) / float64(time.Millisecond),
		"total_allocated_bytes":                   after.TotalAlloc - before.TotalAlloc,
		"heap_alloc_before_bytes":                 before.HeapAlloc,
		"heap_alloc_immediate_after_bytes":        after.HeapAlloc,
		"heap_alloc_after_gc_bytes":               retained.HeapAlloc,
		"retained_heap_delta_bytes":               heapDelta,
		"gc_cycles_during_phase":                  after.NumGC - before.NumGC,
		"linux_rss_before_bytes":                  rssBefore["VmRSS"],
		"linux_rss_immediate_after_bytes":         rssAfter["VmRSS"],
		"linux_process_lifetime_hwm_before_bytes": rssBefore["VmHWM"],
		"linux_process_lifetime_hwm_after_bytes":  rssAfter["VmHWM"],
		"rss_scope": "fresh child lifetime HWM is not an isolated phase peak; current RSS " +
			"includes runtime/index/allocator",
		"clock_scope": "Open includes complete scan/hash and fsync; Inventory copies the " +
			"existing index; setup and body verification excluded",
	}
	if phase == "open" {
		inventory = store.Inventory()
	}
	retainedVerify(t, store, inventory, count, size, os.Getenv("FRY_HOTSTORE_RETAINED_FILE_ID"), expectedSize)
	report["verified_locators"], report["verified_bodies"] = count, count
	retainedWriteReport(t, output, report)
}

func retainedHeapAfterGC(
	t *testing.T,
	before runtime.MemStats,
	store *Store,
	inventory []Locator,
) (runtime.MemStats, int64) {
	t.Helper()
	// This GC is outside the phase clock. The store/index and optional inventory
	// remain live, so this is retained Go heap, not cumulative allocations or RSS.
	var retained runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&retained)
	runtime.KeepAlive(inventory)
	runtime.KeepAlive(store)
	if retained.HeapAlloc > math.MaxInt64 || before.HeapAlloc > math.MaxInt64 {
		t.Fatal("heap measurement exceeds int64")
		return retained, 0
	}
	return retained, int64(retained.HeapAlloc) - int64(before.HeapAlloc)
}

func retainedChildConfig(t *testing.T) (string, int, int, string, string, int64) {
	t.Helper()
	phase := os.Getenv("FRY_HOTSTORE_RETAINED_PHASE")
	if phase == "" {
		t.Skip("private measurement child")
	}
	requireHotCondition(t, (phase == "open" || phase == "inventory"), "unknown measurement phase")
	count, size, err := retainedWorkload()
	requireHotNoError(t, err)
	root := os.Getenv("FRY_HOTSTORE_RETAINED_ROOT")
	output := os.Getenv("FRY_HOTSTORE_RETAINED_CHILD_REPORT")
	requireHotCondition(t, (filepath.IsAbs(root) && filepath.IsAbs(output)), "private absolute fixture paths required")
	expectedSize, err := strconv.ParseInt(os.Getenv("FRY_HOTSTORE_RETAINED_FILE_BYTES"), 10, 64)
	requireHotNoError(t, err)
	return phase, count, size, root, output, expectedSize
}

func TestHotstoreRetainedMeasurement(t *testing.T) {
	if os.Getenv("FRY_HOTSTORE_RETAINED_MEASUREMENT") != "1" {
		t.Skip("opt in with scripts/bench.sh hotstore-retained")
	}
	requireHotCondition(t, os.Getenv("FRY_HOTSTORE_RETAINED_PHASE") == "", "parent/child modes mixed")
	count, size, err := retainedWorkload()
	requireHotNoError(t, err)
	output := os.Getenv("FRY_HOTSTORE_RETAINED_OUTPUT_DIR")
	requireHotCondition(t, filepath.IsAbs(output), "new absolute private output directory required")
	{
		// Explicit opt-in measurement creates a new private operator-selected output directory; never follows
		// existing path; owner: FRY maintainers; expires: 2027-01-18.
		// #nosec G703 -- See the path-safety rationale above.
		err = os.Mkdir(output, 0o700)
		requireHotCondition(t, err == nil, "output must not exist", err)
	}
	fixture, err := os.MkdirTemp("", "fry-hotstore-retained-")
	requireHotNoError(t, err)
	removed := false
	defer func() {
		if !removed {
			if err := os.RemoveAll(fixture); err != nil {
				t.Error("fixture cleanup", err)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	fileID, fileSize := prepareRetainedFixture(t, ctx, fixture, count, size)
	phases := runRetainedPhases(t, ctx, fixture, output, fileID, fileSize)
	hashes := retainedSourceHashes(t)
	removeRetainedFixture(t, fixture)
	removed = true
	filesystemName, executableHash := retainedRuntimeIdentity(t, output)
	report := map[string]any{
		"profile":            "synthetic-hotstore-retained-open-inventory",
		"passed":             true,
		"records":            count,
		"message_bytes":      size,
		"raw_bytes":          int64(count) * int64(size),
		"encoded_file_bytes": fileSize,
		"go":                 runtime.Version(),
		"os":                 runtime.GOOS,
		"arch":               runtime.GOARCH,
		"logical_cpus":       runtime.NumCPU(),
		"gomaxprocs":         runtime.GOMAXPROCS(0),
		"source_sha256":      hashes,
		"phases":             phases,
		"durable_setup":      "real sequential Append+fsync; setup untimed; no retained payload array",
		"cache_scope":        "page cache warmed by setup and subsequent phases; no eviction or cold-disk claim",
		"scope": "one local whole-file hotstore; records are not FRY accepted receipts; " +
			"no PostgreSQL/reclaim/remote/production-capacity claim",
		"fixture_removed":        true,
		"filesystem_type":        filesystemName,
		"filesystem_scope":       "reported filesystem; tmpfs fsync is not persistent-disk durability",
		"gogc_environment":       os.Getenv("GOGC"),
		"gomemlimit_environment": os.Getenv("GOMEMLIMIT"),
		"test_binary_sha256":     hex.EncodeToString(executableHash[:]),
	}
	retainedWriteReport(t, filepath.Join(output, "report.json"), report)
	t.Log("HOTSTORE_RETAINED_MEASUREMENT_PASSED")
}

func prepareRetainedFixture(t *testing.T, ctx context.Context, fixture string, count, size int) (string, int64) {
	t.Helper()
	store, err := Open(fixture, retainedOptions(count, size))
	requireHotNoError(t, err)
	closed := false
	defer func() {
		if !closed {
			closeForTest(t, store)
		}
	}()
	populateRetainedFixture(t, ctx, store, count, size)
	fileID := store.FileID().String()
	info, err := store.file.Stat()
	requireHotNoError(t, err)
	err = store.Close()
	requireHotNoError(t, err)
	closed = true
	return fileID, info.Size()
}

func populateRetainedFixture(t *testing.T, ctx context.Context, store *Store, count, size int) {
	t.Helper()
	// One reused buffer. The parent never retains all payloads or all locators.
	payload := make([]byte, size)
	t.Logf("staging %d durable %d-byte records outside phase clocks", count, size)
	for i := range count {
		retainedPayload(payload, i)
		loc, err := store.Append(ctx, retainedKey(i), bytes.NewReader(payload))
		requireHotCondition(t, err == nil, "durable setup append", err)
		requireHotCondition(
			t,
			((loc.Key == retainedKey(i) && loc.Size == int64(size)) && loc.SHA256 == sha256.Sum256(payload)),
			"setup locator mismatch",
		)
		if (i+1)%8192 == 0 {
			t.Logf("staged %d/%d", i+1, count)
		}
	}
}

func runRetainedPhases(
	t *testing.T,
	ctx context.Context,
	fixture, output, fileID string,
	fileSize int64,
) map[string]json.RawMessage {
	t.Helper()
	phases := map[string]json.RawMessage{}
	for _, phase := range []string{"open", "inventory"} {
		reportPath := filepath.Join(output, phase+".json")
		// os.Executable selects the current test binary; fixed arguments; no shell; owner: FRY maintainers;
		// expires: 2027-01-18.
		// #nosec G204 -- See the path-safety rationale above.
		cmd := exec.CommandContext(
			ctx,
			testExecutable(t),
			"-test.run=^TestHotstoreRetainedMeasurementChild$",
			"-test.count=1",
		)
		cmd.Env = append(
			os.Environ(),
			"FRY_HOTSTORE_RETAINED_PHASE="+phase,
			"FRY_HOTSTORE_RETAINED_ROOT="+fixture,
			"FRY_HOTSTORE_RETAINED_CHILD_REPORT="+reportPath,
			"FRY_HOTSTORE_RETAINED_FILE_ID="+fileID,
			"FRY_HOTSTORE_RETAINED_FILE_BYTES="+strconv.FormatInt(fileSize, 10),
		)
		raw, err := cmd.CombinedOutput()
		requireHotConditionf(t, err == nil, "private %s child failed: %v: %.2048s", phase, err, raw)
		// Fixed phase filename under newly created private operator-selected output directory; owner: FRY
		// maintainers; expires: 2027-01-18.
		// #nosec G304 G703 -- See the path-safety rationale above.
		data, err := os.ReadFile(reportPath)
		requireHotNoError(t, err)
		requireHotCondition(t, json.Valid(data), "invalid child report")
		phases[phase] = data
	}
	return phases
}

func retainedSourceHashes(t *testing.T) map[string]string {
	t.Helper()
	hashes := map[string]string{}
	for _, name := range []string{
		"retained_measurement_linux_test.go",
		"store_linux.go",
		"recovery_linux.go",
		"format.go",
		"../../go.mod",
		"../../go.sum",
		"../../scripts/bench.sh",
		"../../scripts/bench/hotstore_retained.py",
		"../../scripts/lib/benchmark.py",
		"../../scripts/lib/testdeps.py",
	} {
		// Source hash inputs come only from the fixed literal allowlist in this loop; owner: FRY maintainers;
		// expires: 2027-01-18.
		// #nosec G304 -- See the path-safety rationale above.
		raw, err := os.ReadFile(name)
		requireHotCondition(t, err == nil, "source hash", name, err)
		hash := sha256.Sum256(raw)
		hashes[name] = hex.EncodeToString(hash[:])
	}
	return hashes
}

func retainedRuntimeIdentity(t *testing.T, output string) (string, [32]byte) {
	t.Helper()
	var err error
	var filesystem syscall.Statfs_t
	err = syscall.Statfs(output, &filesystem)
	requireHotNoError(t, err)
	filesystemName := fmt.Sprintf("0x%x", filesystem.Type)
	switch filesystem.Type {
	case 0x01021994:
		filesystemName = "tmpfs"
	case 0x9123683e:
		filesystemName = "btrfs"
	case 0xef53:
		filesystemName = "ext"
	case 0x58465342:
		filesystemName = "xfs"
	}
	executable, err := os.ReadFile(testExecutable(t))
	requireHotNoError(t, err)
	executableHash := sha256.Sum256(executable)
	return filesystemName, executableHash
}

func removeRetainedFixture(t *testing.T, fixture string) {
	t.Helper()
	var err error
	{
		err = os.RemoveAll(fixture)
		requireHotCondition(t, err == nil, "fixture cleanup", err)
	}
	{
		_, err = os.Stat(fixture)
		requireHotCondition(t, os.IsNotExist(err), "fixture still exists after cleanup")
	}
}

func retainedLocatorIndex(t *testing.T, loc Locator, count int) int {
	t.Helper()
	if count < 0 || count > 65536 {
		t.Fatal("invalid retained record count")
		return 0
	}
	n := binary.BigEndian.Uint64(loc.Key.Receipt[8:])
	if n == 0 || n > 65536 || n > uint64(count) {
		t.Fatal("unknown retained key")
		return 0
	}
	return int(n - 1)
}
