//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHeaderMetadataBoundsParsingAndAuthority(t *testing.T) {
	f := setup(t)
	releaseCapture := holdHeaderCaptureSlots(f.s)
	defer releaseCapture()
	ctx := context.Background()
	raw := "Subject: original\r\nDate: Tue, 13 Oct 2026 12:00:00 +0000\r\n\r\nbody"
	r := accept(t, f.s, newID(t), f.envelope(t), raw)
	{
		_, _, found, err := f.s.loadHeaderMetadata(ctx, f.org, r.ID)
		requireReceiptCondition(t, err == nil, "new receipt cache state", found, err)
		requireReceiptCondition(t, !found, "new receipt cache state", found, err)
	}
	header, err := f.s.Header(ctx, f.org, r.ID)
	requireReceiptCondition(t, err == nil, string(header), err)
	requireReceiptCondition(
		t,
		string(header) == "Subject: original\r\nDate: Tue, 13 Oct 2026 12:00:00 +0000\r\n\r\n",
		string(header),
		err,
	)
	header[0] = 'x'
	again, err := f.s.Header(ctx, f.org, r.ID)
	requireReceiptCondition(t, err == nil, "caller changed stored header", string(again), err)
	requireReceiptCondition(t, again[0] == 'S', "caller changed stored header", string(again), err)
	requireHeaderMetadataRejectsInvalidUpdates(t, ctx, f, r, again)
	execSQL(
		t,
		f.pool,
		`UPDATE fry_reference SET expires_at=clock_timestamp()-interval '1 second' WHERE `+
			`receipt=$1`,
		r.ID.String(),
	)
	{
		_, err = f.s.Header(ctx, f.org, r.ID)
		requireReceiptCondition(t, errors.Is(err, ErrExpired), "unswept reference expiry served metadata", err)
	}
	{
		err = f.s.storeHeaderMetadata(ctx, f.org, r.ID, again)
		requireReceiptCondition(t, errors.Is(err, ErrExpired), "expired header population revived receipt", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_header_metadata`) == 1,
		"invalid metadata inserted",
	)
}

func TestHeaderMetadataMalformedSourcesNeverPersist(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, body := range []string{
		"not a valid header\r\n\r\nbody",
		"Subject: no boundary",
		"Subject: " + strings.Repeat("x", MaxHeaderBytes) + "\r\n\r\nbody",
	} {
		r := accept(t, f.s, newID(t), f.envelope(t), body)
		for range 2 {
			{
				_, err := f.s.Header(ctx, f.org, r.ID)
				requireReceiptCondition(
					t,
					errors.Is(err, ErrHeaderUnavailable),
					"malformed source became cached success",
					err,
				)
			}
		}
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_header_metadata`) == 0,
		"malformed metadata persisted",
	)
	// A long body does not count against the header bound or enter metadata.
	body := "Subject: bounded\r\n\r\n" + strings.Repeat("body", 300000)
	r := accept(t, f.s, newID(t), f.envelope(t), body)
	{
		got, err := f.s.Header(ctx, f.org, r.ID)
		requireReceiptCondition(t, err == nil, "body entered header metadata", len(got), err)
		requireReceiptCondition(
			t,
			string(got) == "Subject: bounded\r\n\r\n",
			"body entered header metadata",
			len(got),
			err,
		)
	}
}

func TestHeaderPopulationCannotRaceRemovedReceipt(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r := accept(t, f.s, newID(t), f.envelope(t), "Subject: original\r\n\r\nbody")
	header, err := f.s.Header(ctx, f.org, r.ID)
	requireReceiptNoError(t, err)
	expireReceipt(t, f, r)
	for _, m := range f.members {
		err = f.s.ReclaimHot(ctx, m.id)
		requireReceiptNoError(t, err)
	}
	// Model cleanup holding the common receipt guard while a late population
	// request from an earlier header read waits. The committed removed state must
	// fence that request and prevent resurrection after metadata deletion.
	tx, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer rollback(context.Background(), tx)
	_, err = lockReceipt(ctx, tx, f.org, r.ID)
	requireReceiptNoError(t, err)
	done := make(chan error, 1)
	go func() { done <- f.s.storeHeaderMetadata(ctx, f.org, r.ID, header) }()
	// Confirm the writer reached its row-lock wait instead of relying on sleep.
	deadline := time.Now().Add(2 * time.Second)
	for scalar(t, f.pool, `SELECT count(*) FROM pg_stat_activity WHERE `+
		`application_name=current_setting('application_name') AND wait_event_type='Lock' AND `+
		`query LIKE '%FROM fry_receipt WHERE id=$1%FOR UPDATE%'`) == 0 {
		requireReceiptCondition(t, !time.Now().After(deadline), "late population did not wait for receipt guard")
		time.Sleep(time.Millisecond)
	}
	_, err = tx.Exec(ctx, `DELETE FROM fry_header_metadata WHERE receipt=$1`, r.ID.String())
	requireReceiptNoError(t, err)
	_, err = tx.Exec(
		ctx,
		`UPDATE fry_receipt SET lifecycle='removed',removed_at=clock_timestamp() WHERE id=$1`,
		r.ID.String(),
	)
	requireReceiptNoError(t, err)
	err = commit(ctx, tx)
	requireReceiptNoError(t, err)
	{
		err = <-done
		requireReceiptCondition(t, errors.Is(err, ErrExpired), "late population resurrected removed metadata", err)
	}
	{
		_, err = f.s.Header(ctx, f.org, r.ID)
		requireReceiptCondition(t, errors.Is(err, ErrExpired), "removed receipt served metadata", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_header_metadata WHERE receipt=$1`, r.ID.String()) == 0,
		"removed receipt retained header row",
	)
}

func TestHeaderMetadataPersistenceFailureIsNotHidden(t *testing.T) {
	f := setup(t)
	releaseCapture := holdHeaderCaptureSlots(f.s)
	defer releaseCapture()
	ctx := context.Background()
	r := accept(t, f.s, newID(t), f.envelope(t), "Subject: original\r\n\r\nbody")
	execSQL(
		t,
		f.pool,
		`CREATE FUNCTION fail_header_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN `+
			`RAISE EXCEPTION 'synthetic metadata failure'; END $$; CREATE TRIGGER `+
			`fail_header_insert BEFORE INSERT ON fry_header_metadata FOR EACH ROW EXECUTE FUNCTION `+
			`fail_header_insert()`,
	)
	{
		_, err := f.s.Header(ctx, f.org, r.ID)
		requireReceiptCondition(t, err != nil, "metadata persistence error silently ignored")
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_header_metadata`) == 0,
		"failed insert persisted",
	)
	var body bytes.Buffer
	{
		err := f.s.Read(ctx, f.org, r.ID, 0, r.Size, &body)
		requireReceiptCondition(t, err == nil, "derived metadata failure damaged retained original", err)
		requireReceiptCondition(
			t,
			body.String() == "Subject: original\r\n\r\nbody",
			"derived metadata failure damaged retained original",
			err,
		)
	}
}

func requireHeaderMetadataRejectsInvalidUpdates(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	r Receipt,
	again []byte,
) {
	t.Helper()
	var err error
	{
		_, err = f.s.Header(ctx, newID(t), r.ID)
		requireReceiptCondition(t, errors.Is(err, ErrNotFound), "cross-org header read", err)
	}
	{
		err = f.s.storeHeaderMetadata(ctx, newID(t), r.ID, again)
		requireReceiptCondition(t, errors.Is(err, ErrNotFound), "cross-org header insert", err)
	}
	{
		err = f.s.storeHeaderMetadata(ctx, f.org, r.ID, []byte("Subject: changed\r\n\r\n"))
		requireReceiptCondition(t, errors.Is(err, ErrConflict), "immutable metadata replaced", err)
	}
	for _, bad := range [][]byte{
		[]byte("not a header\r\n\r\n"),
		[]byte("Subject: trailing\r\n\r\nbody"),
		[]byte("Subject: unterminated\r\n"),
		append([]byte("Subject: "), bytes.Repeat([]byte("x"), MaxHeaderBytes)...),
	} {
		{
			err = f.s.storeHeaderMetadata(ctx, f.org, r.ID, bad)
			requireReceiptCondition(t, errors.Is(err, ErrHeaderUnavailable), "invalid header persisted", err)
		}
	}
}
