//go:build linux

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/receiptstore"
)

func TestStorageQuotaDecimalValidation(t *testing.T) {
	for _, v := range []string{"", "0", "-1", "+1", " 1", "1.0", "1e3", "9223372036854775808"} {
		{
			_, err := parseStorageLimit(v)
			requireErrorIs(t, err, ErrInvalid)
		}
	}
	for _, v := range []string{"1", "9007199254740993", "9223372036854775807"} {
		n, err := parseStorageLimit(v)
		if err != nil || strconv.FormatInt(n, 10) != v {
			t.Fatalf("lost exact decimal %s", v)
		}
	}
}

func TestStorageQuotaScopeAndAdvisoryTransitions(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	manager := f.manager(t, false)
	third := newID(t)
	check(t, f.service.PutOrganization(ctx, f.root.Token, Organization{third, "Third"}))
	check(t, f.service.ReplaceGrants(ctx, f.root.Token, manager.Identity.ID, []Grant{{Scope: f.org}, {Scope: f.other}}))
	for _, org := range []string{f.org, f.other} {
		check(t, f.service.SetStorageQuota(ctx, manager.Token, org, new("1")))
		if _, err := f.service.StorageUsage(ctx, manager.Token, org); err != nil {
			t.Fatal(err)
		}
	}
	requireErrorIs(t, f.service.SetStorageQuota(ctx, manager.Token, third, new("1")), ErrForbidden)
	{
		_, err := f.service.StorageUsage(ctx, manager.Token, third)
		requireErrorIs(t, err, ErrForbidden)
	}
	check(t, f.service.SetStorageQuota(ctx, f.root.Token, third, new("9223372036854775807")))
	requireErrorIs(t, f.service.SetStorageQuota(ctx, f.root.Token, newID(t), new("1")), ErrInvalid)
	checkStorageTransitions(t, f, manager)
}

func TestStorageQuotaConcurrentSamplesAndFailureRollback(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.receive(t)
	check(t, f.service.SetStorageQuota(ctx, f.root.Token, f.org, new("1")))
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := f.service.EvaluateStorageQuotas(ctx, 1)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	for err := range results {
		check(t, err)
	}
	var n int
	check(t, f.db.QueryRow(ctx, `SELECT count(*) FROM fry_admin_storage_alert`).Scan(&n))
	if n != 1 {
		t.Fatal("duplicate alert", n)
	}
	checkStorageSampleRollback(t, f)
}

func TestStorageQuotaLockedPolicySkipsAndDoesNotBlockAcceptance(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	check(t, f.service.SetStorageQuota(ctx, f.root.Token, f.org, new("1")))
	check(t, f.service.SetStorageQuota(ctx, f.root.Token, f.other, new("1")))
	held, err := f.db.Begin(ctx)
	check(t, err)
	defer rollback(ctx, held)
	_, err = held.Exec(ctx, `SELECT organization FROM fry_admin_storage_quota WHERE organization=$1 FOR UPDATE`, f.org)
	check(t, err)
	n, err := f.service.EvaluateStorageQuotas(ctx, 1)
	check(t, err)
	if n != 1 {
		t.Fatal("locked policy prevented another organization", n)
	}
	other, err := f.service.StorageUsage(ctx, f.root.Token, f.other)
	check(t, err)
	if other.SampledAt == nil {
		t.Fatal("other policy not sampled")
	}
	org, idErr := mailID(f.org)
	check(t, idErr)
	inbox, idErr := mailID(f.inbox)
	check(t, idErr)
	attempt, idErr := mailID(newID(t))
	check(t, idErr)
	_, err = f.mail.Accept(
		ctx,
		attempt,
		receiptstore.Envelope{
			Organization: org,
			MailFrom:     "sender@example.test",
			Destinations: []receiptstore.Destination{{Inbox: inbox, Recipients: []string{"inbox@example.test"}}},
		},
		io.NopCloser(bytes.NewReader([]byte("Subject: over quota\r\n\r\noriginal\r\n"))),
	)
	check(t, err) // The held quota policy never locks the receipt accounting stripes.
}

func TestStorageQuotaDueIndexAndSchemaGate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	check(t, f.service.CheckStorageQuotaSchema(ctx))
	_, err := f.db.Exec(
		ctx,
		`INSERT INTO fry_organization SELECT md5('quota-plan-'||n),'synthetic' FROM `+
			`generate_series(1,2000) n; INSERT INTO `+
			`fry_admin_storage_quota(organization,limit_bytes,next_evaluation) SELECT `+
			`id,1,clock_timestamp()+interval '1 day' FROM fry_organization`,
	)
	check(t, err)
	_, err = f.db.Exec(
		ctx,
		`UPDATE fry_admin_storage_quota SET next_evaluation=clock_timestamp()-interval `+
			`'1 second' WHERE organization=$1`,
		f.org,
	)
	check(t, err)
	_, err = f.db.Exec(ctx, `ANALYZE fry_admin_storage_quota`)
	check(t, err)
	var raw []byte
	check(
		t,
		f.db.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT `+
			`organization,limit_bytes,version,active FROM fry_admin_storage_quota WHERE `+
			`next_evaluation<=statement_timestamp() ORDER BY next_evaluation,organization `+
			`FOR UPDATE SKIP LOCKED LIMIT 1`).
			Scan(&raw),
	)
	var plan any
	check(t, json.Unmarshal(raw, &plan))
	requireDueStorageIndex(t, plan, raw)
	_, err = f.db.Exec(ctx, `ALTER TABLE fry_admin_storage_alert RENAME TO absent_storage_alert`)
	check(t, err)
	if err = f.service.CheckStorageQuotaSchema(ctx); err == nil {
		t.Fatal("missing alert schema passed startup")
	}
}

func requireDueStorageIndex(t *testing.T, plan any, raw []byte) {
	t.Helper()
	found := false
	visitStoragePlan(plan, func(node map[string]any) {
		if node["Index Name"] != "fry_admin_storage_quota_due" {
			return
		}
		condition, ok := node["Index Cond"].(string)
		if ok && strings.Contains(condition, "statement_timestamp()") {
			found = true
		}
		if rows, ok := node["Actual Rows"].(float64); ok && rows > 1 {
			t.Fatal("due index scanned excess rows", rows)
		}
	})
	if !found {
		t.Fatalf("due query lacks stable index range: %s", raw)
	}
}

func visitStoragePlan(value any, inspect func(map[string]any)) {
	switch node := value.(type) {
	case map[string]any:
		inspect(node)
		for _, item := range node {
			visitStoragePlan(item, inspect)
		}
	case []any:
		for _, item := range node {
			visitStoragePlan(item, inspect)
		}
	}
}

func checkStorageTransitions(t *testing.T, f *fixture, manager Session) {
	t.Helper()
	ctx := context.Background()
	_, raw := f.receive(t)
	_, _ = f.receive(t) // over-quota acceptance must still succeed.
	before, err := f.service.StorageUsage(ctx, manager.Token, f.org)
	check(t, err)
	requireUnsampledStorageUsage(t, before, len(raw))
	if n, err := f.service.EvaluateStorageQuotas(ctx, 64); err != nil || n != 3 {
		t.Fatalf("sampling %d %v", n, err)
	}
	sampled, err := f.service.StorageUsage(ctx, manager.Token, f.org)
	check(t, err)
	if !sampled.AlertActive || sampled.SampleStale || sampled.SampledBytes == nil ||
		*sampled.SampledBytes != before.RetainedBytes {
		t.Fatal("missing active sample", sampled)
	}
	checkStoragePolicyChanges(t, f, manager, before.RetainedBytes)
}

func checkStorageSampleRollback(t *testing.T, f *fixture) {
	t.Helper()
	ctx := context.Background()
	check(t, f.service.SetStorageQuota(ctx, f.root.Token, f.org, nil))
	_, err := f.db.Exec(
		ctx,
		`CREATE FUNCTION fail_storage_alert() RETURNS trigger LANGUAGE plpgsql AS `+
			`$$BEGIN RAISE EXCEPTION 'synthetic failure'; END$$; CREATE TRIGGER `+
			`fail_storage_alert BEFORE INSERT ON fry_admin_storage_alert FOR EACH ROW `+
			`EXECUTE FUNCTION fail_storage_alert()`,
	)
	check(t, err)
	if _, err = f.service.EvaluateStorageQuotas(ctx, 1); err == nil {
		t.Fatal("sample failure hidden")
	}
	usage, err := f.service.StorageUsage(ctx, f.root.Token, f.org)
	check(t, err)
	if !usage.AlertActive || !usage.SampleStale {
		t.Fatal("failed sample changed alert/sample")
	}
	_, err = f.db.Exec(ctx, `DROP TRIGGER fail_storage_alert ON fry_admin_storage_alert`)
	check(t, err)
	_, err = f.service.EvaluateStorageQuotas(ctx, 1)
	check(t, err)
	usage, err = f.service.StorageUsage(ctx, f.root.Token, f.org)
	check(t, err)
	if usage.AlertActive || usage.SampleStale {
		t.Fatal("recovery did not resolve")
	}
	_, err = f.db.Exec(ctx, `UPDATE fry_admin_storage_quota SET sampled_at=clock_timestamp()-interval '61 seconds'`)
	check(t, err)
	usage, err = f.service.StorageUsage(ctx, f.root.Token, f.org)
	check(t, err)
	if !usage.SampleStale {
		t.Fatal("old sample fresh")
	}
}

func checkStoragePolicyChanges(t *testing.T, f *fixture, manager Session, retained string) {
	t.Helper()
	ctx := context.Background()
	var err error
	check(t, f.service.SetStorageQuota(ctx, manager.Token, f.org, new(retained))) // equality is within limit.
	updated, err := f.service.StorageUsage(ctx, manager.Token, f.org)
	check(t, err)
	if !updated.AlertActive || !updated.SampleStale {
		t.Fatal("GET cleared old sample or treated policy as fresh")
	}
	_, err = f.service.EvaluateStorageQuotas(ctx, 64)
	check(t, err)
	equal, err := f.service.StorageUsage(ctx, manager.Token, f.org)
	check(t, err)
	if equal.AlertActive {
		t.Fatal("equal threshold exceeded")
	}
	check(t, f.service.SetStorageQuota(ctx, manager.Token, f.org, new("1")))
	_, err = f.service.EvaluateStorageQuotas(ctx, 64)
	check(t, err)
	check(t, f.service.SetStorageQuota(ctx, manager.Token, f.org, nil))
	_, err = f.service.EvaluateStorageQuotas(ctx, 64)
	check(t, err)
	var events int
	check(
		t,
		f.db.QueryRow(ctx, `SELECT count(*) FROM fry_admin_storage_alert WHERE organization=$1`, f.org).Scan(&events),
	)
	if events != 4 {
		t.Fatal("wrong transition count", events)
	}
	var due *string
	check(
		t,
		f.db.QueryRow(ctx, `SELECT next_evaluation::text FROM fry_admin_storage_quota WHERE organization=$1`, f.org).
			Scan(&due),
	)
	if due != nil {
		t.Fatal("disabled policy keeps scanning")
	}
	var audits int
	check(
		t,
		f.db.QueryRow(ctx, `SELECT count(*) FROM fry_admin_audit WHERE action='storage-quota-update' AND `+
			`organization=$1`, f.org).
			Scan(&audits),
	)
	if audits != 4 {
		t.Fatal("policy audit", audits)
	}
}

func requireUnsampledStorageUsage(t *testing.T, before StorageUsage, size int) {
	t.Helper()
	if before.RetainedOriginals != "2" || before.RetainedBytes != strconv.Itoa(2*size) || !before.SampleStale ||
		before.AlertActive {
		t.Fatalf("wrong unsampled usage %+v", before)
	}
}
