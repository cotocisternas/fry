//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// This opt-in measurement uses real accepted originals, not hand-built authority
// rows. Preparation and exact-content verification are outside the drain clock.
func TestMailboxPublicationMeasurement(t *testing.T) {
	if os.Getenv("FRY_PUBLICATION_MEASUREMENT") != "1" {
		t.Skip("opt in with scripts/bench.sh mailbox-publication")
	}
	hashes := map[string]string{}
	for _, name := range []string{
		"mailbox_linux.go",
		"../../db/schema/receiptstore.sql",
		"mailbox_publication_measurement_linux_test.go",
		"../../scripts/bench.sh",
		"../../scripts/bench/mailbox_publication.py",
		"../../scripts/lib/benchmark.py",
		"../../scripts/lib/testdeps.py",
	} {
		raw, err := storageReadFile("../..", filepath.Join("internal/receiptstore", name))
		requireReceiptNoError(t, err)
		sum := sha256.Sum256(raw)
		hashes[name] = hex.EncodeToString(sum[:])
	}
	for _, limit := range []int{1, 16, 64, 128} {
		t.Run(fmt.Sprintf("limit-%d", limit), func(t *testing.T) {
			measureMailboxPublication(t, limit, hashes)
		})
	}
}

const (
	publicationMeasurementCount = 1024
	publicationMeasurementBytes = 1024
)

func measureMailboxPublication(t *testing.T, limit int, hashes map[string]string) {
	t.Helper()
	f := setup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	envelope := f.envelope(t)
	inbox := envelope.Destinations[0].Inbox
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	originals, order := preparePublicationMeasurement(t, ctx, f, envelope, limit)
	var postgresVersion string
	requireReceiptNoError(t, f.pool.QueryRow(ctx, `SELECT version()`).Scan(&postgresVersion))
	calls, elapsed, walBytes := drainPublicationMeasurement(t, ctx, f, limit)
	verifyPublicationOriginals(t, ctx, f, inbox, originals, order)
	verifyPublicationOwnership(t, ctx, f, limit)
	result := map[string]any{
		"profile": "local-preaccepted-mailbox-publication", "limit": limit,
		"messages": publicationMeasurementCount, "message_bytes": publicationMeasurementBytes, "calls": calls,
		"elapsed_ms":                        float64(elapsed) / float64(time.Millisecond),
		"published_per_second":              float64(publicationMeasurementCount) / elapsed.Seconds(),
		"isolated_cluster_wal_insert_bytes": walBytes, "source_sha256": hashes,
		"verified_originals": publicationMeasurementCount, "go_version": runtime.Version(),
		"goos": runtime.GOOS, "goarch": runtime.GOARCH, "logical_cpus": runtime.NumCPU(),
		"postgres_version": postgresVersion, "shared_operation_context_seconds": 180,
	}
	encoded, err := json.Marshal(result)
	requireReceiptNoError(t, err)
	t.Logf("PUBLICATION_MEASUREMENT_JSON %s", encoded)
}

func preparePublicationMeasurement(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	envelope Envelope,
	limit int,
) (map[ID][]byte, []Receipt) {
	t.Helper()
	originals := make(map[ID][]byte, publicationMeasurementCount)
	order := make([]Receipt, 0, publicationMeasurementCount)
	t.Logf(
		"staging %d real %d-byte originals before limit-%d measurement",
		publicationMeasurementCount,
		publicationMeasurementBytes,
		limit,
	)
	for i := range publicationMeasurementCount {
		raw := []byte(fmt.Sprintf("From: fixture@example.test\r\nSubject: publication measurement %04d\r\n\r\n", i))
		raw = append(raw, bytes.Repeat([]byte{'x'}, publicationMeasurementBytes-len(raw)-2)...)
		raw = append(raw, '\r', '\n')
		receipt, err := f.s.Accept(ctx, newID(t), envelope, io.NopCloser(bytes.NewReader(raw)))
		requireReceiptNoError(t, err)
		requireReceiptCondition(t, receipt.Size == publicationMeasurementBytes, "acceptance changed original identity")
		requireReceiptCondition(t, receipt.SHA256 == sha256.Sum256(raw), "acceptance changed original identity")
		originals[receipt.ID] = raw
		order = append(order, receipt)
		if (i+1)%256 == 0 {
			t.Logf("staged %d/%d for limit-%d", i+1, publicationMeasurementCount, limit)
		}
	}
	for query, want := range map[string]int64{
		`SELECT count(*) FROM fry_receipt`: publicationMeasurementCount,
		`SELECT count(*) FROM fry_work`:    publicationMeasurementCount,
		`SELECT count(*) FROM fry_mail`:    0,
		`SELECT count(*) FROM fry_reference WHERE kind='pending-mailbox' AND ` +
			`expires_at IS NULL`: publicationMeasurementCount,
	} {
		{
			got := scalarContext(ctx, t, f.pool, query)
			requireReceiptConditionf(t, got == want, "precondition %s: %d != %d", query, got, want)
		}
	}
	return originals, order
}

func drainPublicationMeasurement(t *testing.T, ctx context.Context, f *fixture, limit int) (int, time.Duration, int64) {
	t.Helper()
	var startLSN, endLSN string
	requireReceiptNoError(t, f.pool.QueryRow(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&startLSN))
	published, calls := 0, 0
	started := time.Now()
	for published < publicationMeasurementCount {
		n, err := f.s.PublishMailboxes(ctx, limit)
		requireReceiptConditionf(t, err == nil, "publication: n=%d err=%v", n, err)
		requireReceiptConditionf(t, n >= 1, "publication: n=%d err=%v", n, err)
		requireReceiptConditionf(t, n <= limit, "publication: n=%d err=%v", n, err)
		published += n
		calls++
		requireReceiptCondition(t, calls <= publicationMeasurementCount, "publication exceeded bounded call count")
	}
	elapsed := time.Since(started)
	requireReceiptNoError(t, f.pool.QueryRow(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&endLSN))
	var walBytes int64
	requireReceiptNoError(
		t,
		f.pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff($1::pg_lsn,$2::pg_lsn)::bigint`, endLSN, startLSN).Scan(&walBytes),
	)
	// All validation below is deliberately outside the drain measurement.
	requireReceiptCondition(
		t,
		published == publicationMeasurementCount,
		"unexpected publication count/calls",
		published,
		calls,
	)
	requireReceiptCondition(
		t,
		calls == publicationMeasurementCount/limit,
		"unexpected publication count/calls",
		published,
		calls,
	)
	return calls, elapsed, walBytes
}

func verifyPublicationOriginals(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	inbox ID,
	originals map[ID][]byte,
	order []Receipt,
) {
	t.Helper()
	folder, mail, err := f.s.Mailbox(ctx, f.org, inbox, "INBOX")
	requireReceiptCondition(t, err == nil, "mailbox allocation", folder, len(mail), err)
	requireReceiptCondition(t, len(mail) == publicationMeasurementCount, "mailbox allocation", folder, len(mail), err)
	requireReceiptCondition(
		t,
		folder.UIDNext == publicationMeasurementCount+1,
		"mailbox allocation",
		folder,
		len(mail),
		err,
	)
	for i, message := range mail {
		requireReceiptConditionf(t, message.UID == uint32(i+1), "UID/receipt/date/flags changed at %d: %+v", i, message)
		requireReceiptConditionf(
			t,
			message.Receipt == order[i].ID,
			"UID/receipt/date/flags changed at %d: %+v",
			i,
			message,
		)
		requireReceiptConditionf(
			t,
			message.Date.Equal(order[i].ReceivedAt),
			"UID/receipt/date/flags changed at %d: %+v",
			i,
			message,
		)
		requireReceiptConditionf(t, len(message.Flags) == 0, "UID/receipt/date/flags changed at %d: %+v", i, message)
		reader, err := f.s.Open(ctx, f.org, message.Receipt, 0, message.Size)
		requireReceiptNoError(t, err)
		raw, readErr := io.ReadAll(io.LimitReader(reader, publicationMeasurementBytes+1))
		closeErr := reader.Close()
		requireReceiptCondition(t, readErr == nil, "retained original mismatch", i, readErr, closeErr)
		requireReceiptCondition(t, closeErr == nil, "retained original mismatch", i, readErr, closeErr)
		requireReceiptCondition(
			t,
			bytes.Equal(raw, originals[message.Receipt]),
			"retained original mismatch",
			i,
			readErr,
			closeErr,
		)
	}
}

func verifyPublicationOwnership(t *testing.T, ctx context.Context, f *fixture, limit int) {
	t.Helper()
	for query, want := range map[string]int64{
		`SELECT count(*) FROM fry_work`: 0,
		`SELECT count(*) FROM fry_reference WHERE kind='mailbox' AND expires_at IS NULL`: publicationMeasurementCount,
		`SELECT count(*) FROM fry_reference WHERE kind='pending-mailbox' AND ` +
			`expires_at<=clock_timestamp()`: publicationMeasurementCount,
		`SELECT count(*) FROM fry_reference_release WHERE kind='pending-mailbox'`: publicationMeasurementCount,
		`SELECT count(*) FROM fry_reference WHERE kind='recovery' AND ` +
			`expires_at>clock_timestamp()`: publicationMeasurementCount,
		`SELECT count(*) FROM fry_mail m JOIN fry_reference r ON r.receipt=m.receipt AND ` +
			`r.kind='mailbox' AND r.reference_key=m.id AND r.expires_at IS NULL`: publicationMeasurementCount,
	} {
		{
			got := scalarContext(ctx, t, f.pool, query)
			requireReceiptConditionf(t, got == want, "ownership %s: %d != %d", query, got, want)
		}
	}
	{
		n, err := f.s.PublishMailboxes(ctx, limit)
		requireReceiptCondition(t, n == 0, "completed work replay changed mailbox", n, err)
		requireReceiptCondition(t, err == nil, "completed work replay changed mailbox", n, err)
	}
}
