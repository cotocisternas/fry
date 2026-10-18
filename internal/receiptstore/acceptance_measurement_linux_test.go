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
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// All state and artifacts here are private synthetic measurement fixtures. No
// profiler, tracing listener, or instrumentation is added to the executable.
const (
	acceptanceMeasurementCount   = 1024
	acceptanceMeasurementBytes   = 65536
	acceptanceMeasurementWorkers = 16
)

type acceptanceHistogram struct {
	Count   int64     `json:"count"`
	Errors  int64     `json:"errors"`
	TotalNS int64     `json:"total_ns"`
	Bins    [64]int64 `json:"histogram_ns_power2"`
}

func (h *acceptanceHistogram) add(elapsed time.Duration, failed bool) {
	h.Count++
	if failed {
		h.Errors++
	}
	h.TotalNS += int64(elapsed)
	n := uint64(max(elapsed.Nanoseconds(), 1))
	h.Bins[bits.Len64(n)-1]++
}

type acceptanceSQL struct {
	Fingerprint string `json:"fingerprint_sha256"`
	Template    string `json:"template"`
	acceptanceHistogram
}
type acceptanceTrace struct {
	enabled  atomic.Bool
	mu       sync.Mutex
	sql      map[string]*acceptanceSQL
	acquire  acceptanceHistogram
	overflow int64
}
type acceptanceTraceKey byte

const (
	acceptanceQueryKey acceptanceTraceKey = iota
	acceptanceAcquireKey
)

type acceptanceQueryStart struct {
	at                    time.Time
	fingerprint, template string
}

// Static labels deliberately avoid storing SQL text, arguments or error values.
// A fingerprint hashes only the statement template, never bound parameters.
func acceptanceTemplate(sql string) string {
	lower := strings.ToLower(strings.Join(strings.Fields(sql), " "))
	for _, rule := range []struct{ prefix, label string }{
		{"begin", "transaction.begin"},
		{"commit", "transaction.commit"},
		{"rollback", "transaction.rollback"},
		{"set local synchronous_commit", "transaction.durable"},
		{"select state from fry_attempt", "attempt.state"},
		{"select ingest_shard from fry_attempt", "attempt.placement"},
		{"select epoch from fry_profile", "profile.share"},
		{"select receipt_id,organization,envelope", "attempt.lock"},
		{"insert into fry_attempt", "attempt.insert"},
		{"select exists(select 1 from fry_ingest_catalog", "placement.catalog"},
		{"select incarnation,failure_domain,maintenance_job", "member.placement"},
		{"select epoch,closed from fry_member_owner", "member.owner"},
		{"insert into fry_target", "target.insert"},
		{"select a.state,a.epoch,a.profile_epoch", "target.validate"},
		{"update fry_target set locator", "target.record_copy"},
		{"select member,incarnation,failure_domain,locator", "target.finalize_inventory"},
		{"insert into fry_receipt", "receipt.insert"},
		{"insert into fry_destination", "destination.insert"},
		{"insert into fry_reference", "reference.insert"},
		{"insert into fry_work", "work.insert"},
		{"update fry_attempt set state='accepted'", "attempt.accept"},
	} {
		if strings.HasPrefix(lower, rule.prefix) {
			return rule.label
		}
	}
	return "other.statement"
}

func (t *acceptanceTrace) TraceQueryStart(
	ctx context.Context,
	_ *pgx.Conn,
	data pgx.TraceQueryStartData,
) context.Context {
	if !t.enabled.Load() {
		return ctx
	}
	normalized := strings.Join(strings.Fields(data.SQL), " ")
	sum := sha256.Sum256([]byte(normalized))
	return context.WithValue(
		ctx,
		acceptanceQueryKey,
		acceptanceQueryStart{
			at:          time.Now(),
			fingerprint: hex.EncodeToString(sum[:]),
			template:    acceptanceTemplate(data.SQL),
		},
	)
}

func (t *acceptanceTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	start, ok := ctx.Value(acceptanceQueryKey).(acceptanceQueryStart)
	if !ok {
		return
	}
	elapsed := time.Since(start.at)
	t.mu.Lock()
	defer t.mu.Unlock()
	bucket := t.sql[start.fingerprint]
	if bucket == nil {
		if len(t.sql) >= 64 {
			t.overflow++
			return
		}
		bucket = &acceptanceSQL{Fingerprint: start.fingerprint, Template: start.template}
		t.sql[start.fingerprint] = bucket
	}
	bucket.add(elapsed, data.Err != nil)
}

func (t *acceptanceTrace) TraceAcquireStart(
	ctx context.Context,
	_ *pgxpool.Pool,
	_ pgxpool.TraceAcquireStartData,
) context.Context {
	if !t.enabled.Load() {
		return ctx
	}
	return context.WithValue(ctx, acceptanceAcquireKey, time.Now())
}

func (t *acceptanceTrace) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	start, ok := ctx.Value(acceptanceAcquireKey).(time.Time)
	if !ok {
		return
	}
	elapsed := time.Since(start)
	t.mu.Lock()
	t.acquire.add(elapsed, data.Err != nil)
	t.mu.Unlock()
}

func (t *acceptanceTrace) snapshot() ([]acceptanceSQL, acceptanceHistogram, int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rows := make([]acceptanceSQL, 0, len(t.sql))
	for _, row := range t.sql {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Fingerprint < rows[j].Fingerprint })
	return rows, t.acquire, t.overflow
}

type acceptanceCapture struct {
	mode           string
	dir            string
	cpu, execution *os.File
	traceWriter    *acceptanceLimitedWriter
	previousMutex  int
	once           sync.Once
	err            error
}
type acceptanceLimitedWriter struct {
	file      *os.File
	remaining int64
	err       error
}

func (w *acceptanceLimitedWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if int64(len(p)) > w.remaining {
		w.err = fmt.Errorf("synthetic trace exceeded64MiB budget")
		return 0, w.err
	}
	n, err := w.file.Write(p)
	w.remaining -= int64(n)
	w.err = err
	return n, err
}

func acceptanceFile(dir, name string) (*os.File, error) {
	return splitStorageFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}

func beginAcceptanceCapture(mode, dir string) (*acceptanceCapture, error) {
	capture := &acceptanceCapture{mode: mode, dir: dir}
	switch mode {
	case "profile":
		f, err := acceptanceFile(dir, "cpu.pprof")
		if err != nil {
			return nil, err
		}
		capture.cpu = f
		if err = pprof.StartCPUProfile(f); err != nil {
			return nil, errors.Join(err, f.Close())
		}
		// The opt-in exact-test runner uses no other profiler. Block sampling returns
		// to disabled; SetMutexProfileFraction supplies its previous setting.
		runtime.SetBlockProfileRate(10000)
		capture.previousMutex = runtime.SetMutexProfileFraction(5)
	case "trace":
		f, err := acceptanceFile(dir, "execution.trace")
		if err != nil {
			return nil, err
		}
		capture.execution = f
		capture.traceWriter = &acceptanceLimitedWriter{file: f, remaining: 64 << 20}
		if err = trace.Start(capture.traceWriter); err != nil {
			return nil, errors.Join(err, f.Close())
		}
	}
	return capture, nil
}

func (c *acceptanceCapture) stop() error {
	c.once.Do(func() {
		switch c.mode {
		case "profile":
			c.stopProfiles()
		case "trace":
			trace.Stop()
			c.err = c.execution.Close()
			if c.err == nil {
				c.err = c.traceWriter.err
			}
		}
	})
	return c.err
}

func acceptanceLatency(samples []time.Duration) map[string]float64 {
	sorted := append([]time.Duration(nil), samples...)
	slices.Sort(sorted)
	result := map[string]float64{}
	for _, p := range []int{50, 95, 99, 100} {
		index := (len(sorted)*p+99)/100 - 1
		result[fmt.Sprintf("p%d", p)] = float64(sorted[index]) / float64(time.Millisecond)
	}
	return result
}

func acceptanceSources(t *testing.T) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, name := range []string{
		"acceptance_measurement_linux_test.go",
		"receiptstore_linux_test.go",
		"store_linux.go",
		"postgres.go",
		"member_linux.go",
		"member_files_linux.go",
		"member_capacity_linux.go",
		"transport_linux.go",
		"migration_linux.go",
		"header_capture_linux.go",
		"storage_usage_linux.go",
		"../../db/schema/receiptstore.sql",
		"../hotstore/store_linux.go",
		"../hotstore/identity_linux.go",
		"../hotstore/limits.go",
		"../hotstore/format.go",
		"../hotstore/recovery_linux.go",
		"../../go.mod",
		"../../go.sum",
		"../../scripts/bench.sh",
		"../../scripts/bench/acceptance.py",
		"../../scripts/lib/benchmark.py",
		"../../scripts/lib/testdeps.py",
	} {
		raw, err := storageReadFile("../..", filepath.Join("internal/receiptstore", name))
		if name == "header_capture_linux.go" && os.IsNotExist(err) {
			continue
		}
		requireReceiptCondition(t, err == nil, "source fingerprint unavailable")
		sum := sha256.Sum256(raw)
		result[name] = hex.EncodeToString(sum[:])
	}
	return result
}

func TestAcceptanceMeasurement(t *testing.T) {
	mode := os.Getenv("FRY_ACCEPTANCE_MEASUREMENT")
	if mode == "" {
		t.Skip("opt in using scripts/bench.sh acceptance")
	}
	requireReceiptCondition(
		t,
		mode == "baseline" || mode == "profile" || mode == "trace",
		"mode must be baseline,profile,or trace",
	)
	output := os.Getenv("FRY_ACCEPTANCE_OUTPUT_DIR")
	requireReceiptCondition(t, filepath.IsAbs(output), "new absolute private output directory required")
	err := storageMkdir(filepath.Dir(output), filepath.Base(output), 0o700)
	requireReceiptCondition(t, err == nil, "cannot create new measurement output directory")
	storageProfile, byteOptions := acceptanceStorageOptions(t)
	tracer := &acceptanceTrace{sql: map[string]*acceptanceSQL{}}
	f := setupWithPoolConfig(t, func(cfg *pgxpool.Config) {
		cfg.MaxConns = 16
		if mode != "baseline" {
			cfg.ConnConfig.Tracer = tracer
		}
	}, byteOptions, Options{Lease: 30 * time.Second, RecoveryRetention: 24 * time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	envelope := f.envelope(t)
	{
		err := f.s.EnsureInbox(ctx, f.org, envelope.Destinations[0].Inbox)
		requireReceiptCondition(t, err == nil, "inbox setup failed")
	}
	prewarmAcceptancePool(t, ctx, f)
	corpus, attempts, hashes := acceptanceMeasurementCorpus(t)
	var postgresVersion string
	{
		err := f.pool.QueryRow(ctx, `SELECT version()`).Scan(&postgresVersion)
		requireReceiptCondition(t, err == nil, "cannot read fixture database version")
	}
	sourceHashes := acceptanceSources(t)
	run := runAcceptanceMeasurement(t, ctx, cancel, f, envelope, mode, output, tracer, corpus, attempts)
	sql, acquire, overflow := tracer.snapshot()
	filesystemType := acceptanceMemberFilesystem(t, f)
	report := acceptanceMeasurementReport(
		run,
		mode,
		filesystemType,
		postgresVersion,
		storageProfile,
		sourceHashes,
		byteOptions,
		sql,
		acquire,
		overflow,
	)
	requireAcceptanceMeasurementComplete(t, run, overflow)
	addAcceptanceHeaderCoverage(t, ctx, f, run.succeeded, report)
	// Verification, profile finalization and report writes are outside the clock.
	// Diagnostic CPU/trace streaming writes occur during acceptance and add overhead.
	verifyAcceptanceOriginals(t, ctx, f, run.receipts, attempts, hashes, corpus)
	verifyAcceptanceAuthority(t, f)
	report["verified_receipts"] = acceptanceMeasurementCount
	report["verified_bodies"] = acceptanceMeasurementCount
	addAcceptanceCapacityVerification(t, ctx, f, storageProfile, byteOptions, report)
	writeAcceptanceMeasurementReport(t, output, report)
}

func TestAcceptanceTraceBoundedAndRedacted(t *testing.T) {
	tracer := &acceptanceTrace{sql: map[string]*acceptanceSQL{}}
	tracer.enabled.Store(true)
	for i := range 65 {
		ctx := tracer.TraceQueryStart(
			context.Background(),
			nil,
			pgx.TraceQueryStartData{SQL: fmt.Sprintf("SELECT $%d", i+1), Args: []any{"synthetic-secret-never-record"}},
		)
		tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: fmt.Errorf("synthetic-error-never-record")})
	}
	ctx := tracer.TraceAcquireStart(context.Background(), nil, pgxpool.TraceAcquireStartData{})
	tracer.TraceAcquireEnd(ctx, nil, pgxpool.TraceAcquireEndData{})
	sql, acquire, overflow := tracer.snapshot()
	requireReceiptCondition(t, len(sql) == 64, "unbounded or missing aggregation")
	requireReceiptCondition(t, overflow == 1, "unbounded or missing aggregation")
	requireReceiptCondition(t, acquire.Count == 1, "unbounded or missing aggregation")
	requireReceiptCondition(t, acquire.Errors == 0, "unbounded or missing aggregation")
	for _, bucket := range sql {
		requireReceiptCondition(t, bucket.Count == 1, "unexpected aggregation")
		requireReceiptCondition(t, bucket.Errors == 1, "unexpected aggregation")
		requireReceiptCondition(t, bucket.Template == "other.statement", "unexpected aggregation")
	}
	raw, err := json.Marshal(sql)
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, !bytes.Contains(raw, []byte("synthetic-secret")), "trace disclosed args,error,or SQL")
	requireReceiptCondition(t, !bytes.Contains(raw, []byte("synthetic-error")), "trace disclosed args,error,or SQL")
	requireReceiptCondition(t, !bytes.Contains(raw, []byte("SELECT")), "trace disclosed args,error,or SQL")
	tracer.enabled.Store(false)
	ctx = tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT $100"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	_, _, after := tracer.snapshot()
	requireReceiptCondition(t, after == overflow, "disabled trace recorded work")
}

func acceptanceStorageOptions(t *testing.T) (string, hotstore.Options) {
	t.Helper()
	storageProfile := os.Getenv("FRY_ACCEPTANCE_STORAGE_PROFILE")
	if storageProfile == "" {
		storageProfile = "legacy"
	}
	byteOptions := hotstore.Options{
		MaxMessageBytes:      acceptanceMeasurementBytes,
		MaxBytes:             256 << 20,
		MaxConcurrentAppends: acceptanceMeasurementWorkers,
	}
	switch os.Getenv("FRY_ACCEPTANCE_MAX_MESSAGE_BYTES") {
	case "", "65536":
	case "1048576":
		byteOptions.MaxMessageBytes = 1 << 20
	default:
		t.Fatal("fixed measurement maximum must be 65536 or 1048576")
	}
	switch storageProfile {
	case "legacy":
	case "bounded":
		byteOptions.MaxRecords = 2048
		byteOptions.MaxFileBytes = 96 << 20
	default:
		t.Fatal("storage profile must be legacy or bounded")
	}
	return storageProfile, byteOptions
}

func prewarmAcceptancePool(t *testing.T, ctx context.Context, f *fixture) {
	t.Helper()
	// Hold sixteen acquisitions together to prewarm exactly the root-sized pool.
	var warm []*pgxpool.Conn
	for range 16 {
		conn, err := f.pool.Acquire(ctx)
		if err != nil {
			for _, c := range warm {
				c.Release()
			}
			t.Fatal("pool warmup failed")
		}
		warm = append(warm, conn)
	}
	for _, conn := range warm {
		conn.Release()
	}
}

func acceptanceMeasurementCorpus(t *testing.T) ([][]byte, []ID, [][32]byte) {
	t.Helper()
	corpus := make([][]byte, acceptanceMeasurementCount)
	attempts := make([]ID, acceptanceMeasurementCount)
	hashes := make([][32]byte, acceptanceMeasurementCount)
	for i := range corpus {
		prefix := []byte(fmt.Sprintf("From: synthetic@example.test\r\nSubject: accept measurement %04d\r\n\r\n", i))
		corpus[i] = append(prefix, bytes.Repeat([]byte{'x'}, acceptanceMeasurementBytes-len(prefix)-2)...)
		corpus[i] = append(corpus[i], '\r', '\n')
		attempts[i] = newID(t)
		hashes[i] = sha256.Sum256(corpus[i])
	}
	return corpus, attempts, hashes
}

func acceptanceMemberFilesystem(t *testing.T, f *fixture) string {
	t.Helper()
	// Inspect the actual member root, not the repository or report directory.
	// The runner places its scratch beneath the requested output, which can be
	// memory-backed. This observation is outside the acceptance interval.
	var filesystem syscall.Statfs_t
	{
		err := syscall.Statfs(f.dirs[0], &filesystem)
		requireReceiptCondition(t, err == nil, "member filesystem metadata unavailable")
	}
	filesystemType := fmt.Sprintf("0x%x", filesystem.Type)
	switch filesystem.Type {
	case 0x01021994:
		filesystemType = "tmpfs"
	case 0x9123683e:
		filesystemType = "btrfs"
	case 0xef53:
		filesystemType = "ext"
	case 0x58465342:
		filesystemType = "xfs"
	}
	return filesystemType
}

func addAcceptanceHeaderCoverage(t *testing.T, ctx context.Context, f *fixture, succeeded int, report map[string]any) {
	t.Helper()
	var err error
	// Observe capture coverage before any content verification can lazily populate
	// metadata. This query and its allocation are outside all acceptance capture.
	var capturedHeaders, capturedHeaderBytes int64
	{
		err = f.pool.QueryRow(
			ctx,
			`SELECT count(h.receipt),COALESCE(sum(octet_length(h.raw_header)),0) FROM fry_receipt `+
				`r LEFT JOIN fry_header_metadata h ON h.receipt=r.id WHERE r.organization=$1`,
			f.org.String(),
		).
			Scan(&capturedHeaders, &capturedHeaderBytes)
		requireReceiptCondition(t, err == nil, "header capture coverage unavailable")
	}
	requireReceiptCondition(t, capturedHeaders >= 0, "header capture coverage outside bounds")
	requireReceiptCondition(t, capturedHeaders <= int64(succeeded), "header capture coverage outside bounds")
	requireReceiptCondition(t, capturedHeaderBytes >= 0, "header capture coverage outside bounds")
	requireReceiptCondition(
		t,
		capturedHeaderBytes <= capturedHeaders*MaxHeaderBytes,
		"header capture coverage outside bounds",
	)
	report["captured_header_receipts"] = capturedHeaders
	report["captured_header_bytes"] = capturedHeaderBytes
	report["uncaptured_header_receipts"] = int64(succeeded) - capturedHeaders
	report["header_capture_coverage_scope"] = "organization receipt metadata immediately after " +
		"acceptance, before content " +
		"verification; outside timer/profiles/SQL tracer; optional capture may skip"
}

func verifyAcceptanceOriginals(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	receipts []Receipt,
	attempts []ID,
	hashes [][32]byte,
	corpus [][]byte,
) {
	t.Helper()
	for i, receipt := range receipts {
		status, err := f.s.Status(ctx, f.org, attempts[i])
		requireReceiptConditionf(t, err == nil, "receipt identity verification failed at%d", i)
		requireReceiptConditionf(t, status.State == "accepted", "receipt identity verification failed at%d", i)
		requireReceiptConditionf(t, status.Receipt != nil, "receipt identity verification failed at%d", i)
		requireReceiptConditionf(t, status.Receipt.ID == receipt.ID, "receipt identity verification failed at%d", i)
		requireReceiptConditionf(t, receipt.SHA256 == hashes[i], "receipt identity verification failed at%d", i)
		requireReceiptConditionf(
			t,
			receipt.Size == acceptanceMeasurementBytes,
			"receipt identity verification failed at%d",
			i,
		)
		reader, err := f.s.Open(ctx, f.org, receipt.ID, 0, receipt.Size)
		requireReceiptConditionf(t, err == nil, "receipt read failed at%d", i)
		body, readErr := io.ReadAll(io.LimitReader(reader, acceptanceMeasurementBytes+1))
		closeErr := reader.Close()
		requireReceiptConditionf(t, readErr == nil, "receipt body verification failed at%d", i)
		requireReceiptConditionf(t, closeErr == nil, "receipt body verification failed at%d", i)
		requireReceiptConditionf(t, bytes.Equal(body, corpus[i]), "receipt body verification failed at%d", i)
	}
}

func verifyAcceptanceAuthority(t *testing.T, f *fixture) {
	t.Helper()
	for query, want := range map[string]int64{
		`SELECT count(*) FROM fry_attempt WHERE state='accepted'`: acceptanceMeasurementCount,
		`SELECT count(*) FROM fry_receipt`:                        acceptanceMeasurementCount,
		`SELECT count(*) FROM fry_target WHERE locator IS NOT NULL AND ` +
			`NOT bytes_removed`: 2 * acceptanceMeasurementCount,
		`SELECT count(*) FROM fry_work`: acceptanceMeasurementCount,
		`SELECT count(*) FROM fry_mail`: 0,
		`SELECT count(*) FROM fry_reference WHERE kind='pending-mailbox' AND ` +
			`expires_at IS NULL`: acceptanceMeasurementCount,
		`SELECT count(*) FROM fry_reference WHERE kind='recovery' AND ` +
			`expires_at>clock_timestamp()`: acceptanceMeasurementCount,
		`SELECT count(*) FROM fry_read_pin`: 0,
	} {
		{
			n := scalar(t, f.pool, query)
			requireReceiptCondition(t, n == want, "acceptance authority verification failed")
		}
	}
}

func addAcceptanceCapacityVerification(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	storageProfile string,
	byteOptions hotstore.Options,
	report map[string]any,
) {
	t.Helper()
	if storageProfile == "bounded" {
		encoded, err := hotstore.EncodedRecordBytes(acceptanceMeasurementBytes)
		requireReceiptNoError(t, err)
		maximumEncoded, err := hotstore.EncodedRecordBytes(byteOptions.MaxMessageBytes)
		requireReceiptNoError(t, err)
		wantLanes := min(int64(16), int64(byteOptions.MaxConcurrentAppends), byteOptions.MaxRecords,
			max(int64(1), (byteOptions.MaxFileBytes-memberFileHeaderBytes)/maximumEncoded/8))
		exactFiles := verifyAcceptanceCapacityFileOccupancy(t, ctx, f, byteOptions, encoded, wantLanes)
		var exactTargets, exactLanes int64
		{
			err := f.pool.QueryRow(
				ctx,
				`SELECT count(*) FROM fry_target WHERE capacity_settled AND capacity_reserved_bytes=$1 `+
					`AND capacity_lane IS NOT NULL`,
				encoded,
			).
				Scan(&exactTargets)
			requireReceiptCondition(t, err == nil, "bounded target settlement verification failed", err)
			requireReceiptCondition(
				t,
				exactTargets == 2*acceptanceMeasurementCount,
				"bounded target settlement verification failed",
				err,
			)
		}
		{
			err := f.pool.QueryRow(ctx, `SELECT count(*) FROM fry_member_file_lane l
 JOIN fry_member_file f USING(member,file_id)
 CROSS JOIN LATERAL (SELECT count(*) n,COALESCE(sum(capacity_reserved_bytes),0) bytes
 FROM fry_target t WHERE t.member=l.member AND t.incarnation=l.file_id AND t.capacity_lane=l.lane) targets
 WHERE f.state='current' AND l.reserved_records=targets.n AND l.reserved_bytes=targets.bytes`).Scan(&exactLanes)
			requireReceiptCondition(t, err == nil, "bounded per-lane target census verification failed", err)
			requireReceiptCondition(
				t,
				exactLanes == 2*wantLanes,
				"bounded per-lane target census verification failed",
				err,
			)
		}
		report["verified_capacity_files"] = exactFiles
		report["verified_capacity_targets"] = exactTargets
		report["verified_capacity_lanes"] = exactLanes
		report["capacity_lanes_per_file"] = wantLanes
		report["capacity_lane_version"] = 1
	}
}

func writeAcceptanceMeasurementReport(t *testing.T, output string, report map[string]any) {
	t.Helper()
	raw, err := json.Marshal(report)
	requireReceiptCondition(t, err == nil, "cannot encode private measurement")
	file, err := acceptanceFile(output, "report.json")
	requireReceiptCondition(t, err == nil, "cannot create private report")
	_, writeErr := file.Write(append(raw, '\n'))
	closeErr := file.Close()
	requireReceiptCondition(t, writeErr == nil, "cannot write private report")
	requireReceiptCondition(t, closeErr == nil, "cannot write private report")
	t.Logf("ACCEPTANCE_MEASUREMENT_JSON=%s", raw)
}

func (c *acceptanceCapture) stopProfiles() {
	runtime.SetBlockProfileRate(0)
	runtime.SetMutexProfileFraction(c.previousMutex)
	pprof.StopCPUProfile()
	c.err = c.cpu.Close()
	for _, name := range []string{"block", "mutex"} {
		f, err := acceptanceFile(c.dir, name+".pprof")
		if err == nil {
			err = pprof.Lookup(name).WriteTo(f, 0)
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
		}
		if c.err == nil {
			c.err = err
		}
	}
}

func runAcceptanceMeasurement(
	t *testing.T,
	ctx context.Context,
	cancel context.CancelFunc,
	f *fixture,
	envelope Envelope,
	mode, output string,
	tracer *acceptanceTrace,
	corpus [][]byte,
	attempts []ID,
) acceptanceMeasurementResult {
	t.Helper()
	run := acceptanceMeasurementResult{
		receipts:  make([]Receipt, acceptanceMeasurementCount),
		failures:  make([]error, acceptanceMeasurementCount),
		latencies: make([]time.Duration, acceptanceMeasurementCount),
	}
	wg, start, attempted := launchAcceptanceWorkers(
		t, ctx, f, envelope, corpus, attempts, run.receipts, run.failures, run.latencies,
	)
	capture, err := beginAcceptanceCapture(mode, output)
	if err != nil {
		cancel()
		close(start)
		wg.Wait()
		t.Fatal("cannot start private capture")
	}
	defer func() {
		if err := capture.stop(); err != nil {
			t.Error(err)
		}
	}()
	run.beforePool = f.pool.Stat()
	runtime.ReadMemStats(&run.beforeMemory)
	tracer.enabled.Store(mode != "baseline")
	began := time.Now()
	close(start)
	wg.Wait()
	run.elapsed = time.Since(began)
	tracer.enabled.Store(false)
	runtime.ReadMemStats(&run.afterMemory)
	run.afterPool = f.pool.Stat()
	{
		err = capture.stop()
		requireReceiptCondition(t, err == nil, "private capture failed")
	}
	for _, err := range run.failures {
		if err == nil {
			run.succeeded++
		}
	}
	run.attempted = attempted.Load()
	return run
}

type acceptanceMeasurementResult struct {
	receipts                  []Receipt
	failures                  []error
	latencies                 []time.Duration
	attempted                 int64
	elapsed                   time.Duration
	beforePool, afterPool     *pgxpool.Stat
	beforeMemory, afterMemory runtime.MemStats
	succeeded                 int
}

func acceptanceMeasurementReport(
	run acceptanceMeasurementResult,
	mode, filesystemType, postgresVersion, storageProfile string,
	sourceHashes map[string]string,
	byteOptions hotstore.Options,
	sql []acceptanceSQL,
	acquire acceptanceHistogram,
	overflow int64,
) map[string]any {
	return map[string]any{
		"scope": "isolated public Store.Accept; one local pair on shared filesystem; no " +
			"SMTP,publication,or IMAP worker; not production capacity",
		"member_filesystem_type": filesystemType,
		"member_filesystem_scope": "actual local member root; tmpfs fsync is not " +
			"persistent-disk durability evidence; " +
			"PostgreSQL storage is separate",
		"mode":                mode,
		"messages":            acceptanceMeasurementCount,
		"message_bytes":       acceptanceMeasurementBytes,
		"workers":             acceptanceMeasurementWorkers,
		"attempted":           run.attempted,
		"succeeded":           run.succeeded,
		"errors":              len(run.failures) - run.succeeded,
		"elapsed_ms":          float64(run.elapsed) / float64(time.Millisecond),
		"accepted_per_second": float64(run.succeeded) / run.elapsed.Seconds(),
		"latency_ms":          acceptanceLatency(run.latencies),
		"pool":                acceptancePoolReport(run),
		"process_allocated_bytes_during_acceptance": run.afterMemory.TotalAlloc - run.beforeMemory.TotalAlloc,
		"process_gc_cycles_during_acceptance":       run.afterMemory.NumGC - run.beforeMemory.NumGC,
		"sql_trace_enabled":                         mode != "baseline",
		"runtime_profiles_enabled":                  mode == "profile",
		"runtime_trace_enabled":                     mode == "trace",
		"sql":                                       sql,
		"acquire_trace":                             acquire,
		"sql_bucket_overflow":                       overflow,
		"sql_timing_note": "overlapping concurrent durations are not an additive wall-clock breakdown; histogram " +
			"bins are powers of two nanoseconds; no args or SQL text recorded",
		"allocation_scope": "Go test process during acceptance, including tracer/profiler/background goroutines; " +
			"PostgreSQL excluded",
		"postgresql_version":         postgresVersion,
		"go":                         runtime.Version(),
		"os":                         runtime.GOOS,
		"arch":                       runtime.GOARCH,
		"logical_cpus":               runtime.NumCPU(),
		"gomaxprocs":                 runtime.GOMAXPROCS(0),
		"source_sha256":              sourceHashes,
		"prewarmed_pool_connections": 16,
		"diagnostic_streaming_note": "CPU/trace streaming and SQL/acquire tracing overhead occur inside the timed " +
			"diagnostic runs; final profile flush/report/verification are outside",
		"lease_seconds":          30,
		"max_concurrent_appends": 16,
		"max_message_bytes":      byteOptions.MaxMessageBytes,
		"member_capacity_bytes":  byteOptions.MaxBytes,
		"storage_profile":        storageProfile,
		"segment_max_records":    byteOptions.MaxRecords,
		"segment_max_bytes":      byteOptions.MaxFileBytes,
		"profile_block_rate_ns":  map[bool]int{true: 10000, false: 0}[mode == "profile"],
		"profile_mutex_fraction": map[bool]int{true: 5, false: 0}[mode == "profile"],
		"trace_budget_bytes":     64 << 20,
	}
}

func launchAcceptanceWorkers(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	envelope Envelope,
	corpus [][]byte,
	attempts []ID,
	receipts []Receipt,
	failures []error,
	latencies []time.Duration,
) (*sync.WaitGroup, chan struct{}, *atomic.Int64) {
	t.Helper()
	var next atomic.Int64
	var attempted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	ready := make(chan struct{}, acceptanceMeasurementWorkers)
	for range acceptanceMeasurementWorkers {
		wg.Go(func() {
			ready <- struct{}{}
			<-start
			for {
				index := int(next.Add(1)) - 1
				if index >= acceptanceMeasurementCount {
					return
				}
				attempted.Add(1)
				begin := time.Now()
				receipts[index], failures[index] = f.s.Accept(
					ctx,
					attempts[index],
					envelope,
					io.NopCloser(bytes.NewReader(corpus[index])),
				)
				latencies[index] = time.Since(begin)
			}
		})
	}
	for range acceptanceMeasurementWorkers {
		<-ready
	}
	return &wg, start, &attempted
}

func requireAcceptanceMeasurementComplete(t *testing.T, run acceptanceMeasurementResult, overflow int64) {
	t.Helper()
	requireReceiptConditionf(
		t,
		run.succeeded == acceptanceMeasurementCount,
		"acceptance measurement incomplete: attempted%d succeeded%d errors%d overflow%d",
		run.attempted,
		run.succeeded,
		len(run.failures)-run.succeeded,
		overflow,
	)
	requireReceiptConditionf(
		t,
		run.attempted == acceptanceMeasurementCount,
		"acceptance measurement incomplete: attempted%d succeeded%d errors%d overflow%d",
		run.attempted,
		run.succeeded,
		len(run.failures)-run.succeeded,
		overflow,
	)
	requireReceiptConditionf(
		t,
		overflow == 0,
		"acceptance measurement incomplete: attempted%d succeeded%d errors%d overflow%d",
		run.attempted,
		run.succeeded,
		len(run.failures)-run.succeeded,
		overflow,
	)
}

func verifyAcceptanceCapacityFileOccupancy(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	byteOptions hotstore.Options,
	encoded, wantLanes int64,
) int64 {
	t.Helper()
	var exactFiles int64
	{
		err := f.pool.QueryRow(
			ctx,
			`SELECT count(*) FROM fry_member_file f
 JOIN fry_member_profile p USING(member)
 CROSS JOIN LATERAL (SELECT count(*) n,sum(record_ceiling) rc,sum(byte_ceiling) bc,
 sum(reserved_records) records,sum(reserved_bytes) bytes FROM fry_member_file_lane l
 WHERE l.member=f.member AND l.file_id=f.file_id) usage
 WHERE f.state='current' AND f.charged_bytes=$3 AND p.lane_version=1 AND p.lane_count=$4
 AND usage.n=$4 AND usage.rc=$5 AND usage.bc=$3-64
 AND usage.records=$1 AND usage.bytes=$2`,
			acceptanceMeasurementCount,
			acceptanceMeasurementCount*encoded,
			byteOptions.MaxFileBytes,
			wantLanes,
			byteOptions.MaxRecords,
		).Scan(&exactFiles)
		requireReceiptCondition(t, err == nil, "bounded file/lane occupancy verification failed", err)
		requireReceiptCondition(t, exactFiles == 2, "bounded file/lane occupancy verification failed", err)
	}
	return exactFiles
}

func acceptancePoolReport(run acceptanceMeasurementResult) map[string]any {
	return map[string]any{
		"max_connections":   run.afterPool.MaxConns(),
		"acquires":          run.afterPool.AcquireCount() - run.beforePool.AcquireCount(),
		"acquire_total_ns":  run.afterPool.AcquireDuration() - run.beforePool.AcquireDuration(),
		"empty_acquires":    run.afterPool.EmptyAcquireCount() - run.beforePool.EmptyAcquireCount(),
		"empty_wait_ns":     run.afterPool.EmptyAcquireWaitTime() - run.beforePool.EmptyAcquireWaitTime(),
		"canceled_acquires": run.afterPool.CanceledAcquireCount() - run.beforePool.CanceledAcquireCount(),
		"new_connections":   run.afterPool.NewConnsCount() - run.beforePool.NewConnsCount(),
	}
}
