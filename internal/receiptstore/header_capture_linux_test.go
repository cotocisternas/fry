//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Occupying the real optional budget explicitly exercises legacy lazy fallback.
func holdHeaderCaptureSlots(s *Store) func() {
	for range cap(s.headerCaptures) {
		s.headerCaptures <- struct{}{}
	}
	return func() {
		for range cap(s.headerCaptures) {
			<-s.headerCaptures
		}
	}
}

func captureTestStore() *Store {
	return &Store{headerCaptures: make(chan struct{}, 16), headerParses: make(chan struct{}, 1)}
}

func TestHeaderCaptureEveryBoundaryAndFullStreamProof(t *testing.T) {
	for _, header := range []string{
		"\n",
		"\r\n",
		"Subject: x\n\n",
		"Subject: x\r\n\r\n",
		"Subject: x\r\n Folded\r\n\r\n",
	} {
		raw := header + strings.Repeat("private body", 10000)
		for split := 0; split <= len(header)+1; split++ {
			s := captureTestStore()
			c := s.beginHeaderCapture()
			c.observe([]byte(raw[:split]))
			c.observe([]byte(raw[split:]))
			if string(c.raw) != header || cap(c.raw) > MaxHeaderBytes {
				t.Fatal("body retained or boundary missed", header, split, len(c.raw), cap(c.raw))
			}
			proof := s.finishHeaderCapture(c)
			requireReceiptCondition(
				t,
				proof != nil && string(
					proof.raw,
				) == header && proof.size == int64(
					len(raw),
				) && proof.sha256 == sha256.Sum256(
					[]byte(raw),
				) &&
					c.raw == nil,
				"stream proof or parser transfer",
				header,
				split,
			)
		}
	}
}

func TestHeaderCaptureBoundsAndOptionalParsing(t *testing.T) {
	for _, n := range []int{MaxHeaderBytes - 1, MaxHeaderBytes, MaxHeaderBytes + 1} {
		s := captureTestStore()
		c := s.beginHeaderCapture()
		header := "Subject: " + strings.Repeat("x", n-len("Subject: \r\n\r\n")) + "\r\n\r\n"
		for off := 0; off < len(header); off += 997 {
			c.observe([]byte(header[off:min(off+997, len(header))]))
			requireReceiptCondition(t, cap(c.raw) <= MaxHeaderBytes, "allocation exceeded bound")
		}
		c.observe(bytes.Repeat([]byte("body"), 300000))
		proof := s.finishHeaderCapture(c)
		if (proof != nil) != (n <= MaxHeaderBytes) {
			t.Fatal("near-cap decision", n, proof != nil)
		}
		if proof != nil && cap(proof.raw) > MaxHeaderBytes {
			t.Fatal("retained proof allocation exceeded bound", cap(proof.raw))
		}
	}
	for _, raw := range []string{"Subject: unterminated", "invalid header\r\n\r\nbody"} {
		s := captureTestStore()
		c := s.beginHeaderCapture()
		c.observe([]byte(raw))
		requireReceiptCondition(t, s.finishHeaderCapture(c) == nil, "invalid header captured")
	}
}

func TestHeaderCaptureEagerAndBudgetFallback(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	raw := "Subject: private\r\n\r\nsecret body"
	r := accept(t, f.s, newID(t), f.envelope(t), raw)
	header, _, found, err := f.s.loadHeaderMetadata(ctx, f.org, r.ID)
	if err != nil || !found || string(header) != "Subject: private\r\n\r\n" {
		t.Fatal("eager metadata", string(header), found, err)
	}
	for _, budget := range []string{"capture", "parse"} {
		var release func()
		if budget == "capture" {
			release = holdHeaderCaptureSlots(f.s)
		} else {
			f.s.headerParses <- struct{}{}
			release = func() { <-f.s.headerParses }
		}
		r = accept(t, f.s, newID(t), f.envelope(t), raw)
		release()
		if _, _, found, err = f.s.loadHeaderMetadata(ctx, f.org, r.ID); err != nil || found {
			t.Fatal("exhausted optional budget", budget, found, err)
		}
		if header, err = f.s.Header(ctx, f.org, r.ID); err != nil || string(header) != "Subject: private\r\n\r\n" {
			t.Fatal("lazy fallback", budget, err)
		}
	}
	requireReceiptCondition(t, len(f.s.headerCaptures) == 0, "permits leaked")
	requireReceiptCondition(t, len(f.s.headerParses) == 0, "permits leaked")
}

type headerUnreadSource struct{ read bool }

func (r *headerUnreadSource) Read([]byte) (int, error) {
	r.read = true
	return 0, errors.New("must not read")
}
func (*headerUnreadSource) Close() error { return nil }

func TestHeaderCaptureAcceptedRetryIgnoresReplacementSource(t *testing.T) {
	f := setup(t)
	attempt, env := newID(t), f.envelope(t)
	r := accept(t, f.s, attempt, env, "Subject: original\r\n\r\nbody")
	replacement := &headerUnreadSource{}
	again, err := f.s.Accept(context.Background(), attempt, env, replacement)
	if err != nil || again != r || replacement.read || len(f.s.headerCaptures) != 0 {
		t.Fatal("accepted retry consumed source or permit", err, replacement.read)
	}
	header, err := f.s.Header(context.Background(), f.org, r.ID)
	if err != nil || string(header) != "Subject: original\r\n\r\n" {
		t.Fatal(string(header), err)
	}
}

func TestHeaderCaptureProofMismatchAndSourceFreeRecovery(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	c := prepare(t, f, newID(t), f.envelope(t))
	raw := "Subject: proof\r\n\r\nbody"
	copies(t, f, c, raw)
	proof := &capturedHeader{
		raw:    []byte("Subject: proof\r\n\r\n"),
		size:   int64(len(raw)),
		sha256: sha256.Sum256([]byte(raw)),
	}
	for _, change := range []string{"size", "digest"} {
		bad := *proof
		if change == "size" {
			bad.size++
		} else {
			bad.sha256[0]++
		}
		if _, err := f.s.db.finalizeWithHeader(ctx, c, &bad); !errors.Is(err, ErrConflict) {
			t.Fatal("mismatched stream accepted", change, err)
		}
		requireReceiptCondition(
			t,
			scalar(t, f.pool, `SELECT count(*) FROM fry_receipt`) == 0 &&
				scalar(t, f.pool, `SELECT count(*) FROM fry_header_metadata`) == 0,
			"mismatch published authority",
		)
	}
	r, err := f.s.db.finalize(ctx, c)
	requireReceiptNoError(t, err)
	if _, _, found, err := f.s.loadHeaderMetadata(ctx, f.org, r.ID); err != nil || found {
		t.Fatal(found, err)
	}
	if _, err = f.s.Header(ctx, f.org, r.ID); err != nil {
		t.Fatal("source-free lazy recovery", err)
	}
}

func TestHeaderCaptureFailuresReleaseBudgetAndRollbackMetadata(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	raw := "Subject: fail\r\n\r\nbody"
	f.s.afterCopies = func() error { return io.ErrUnexpectedEOF }
	if _, err := f.s.Accept(ctx, newID(t), f.envelope(t), io.NopCloser(strings.NewReader(raw))); err == nil {
		t.Fatal("copy interruption accepted")
	}
	f.s.afterCopies = nil
	f.s.db.beforeCommit = func(pgx.Tx) error { return io.ErrUnexpectedEOF }
	if _, err := f.s.Accept(ctx, newID(t), f.envelope(t), io.NopCloser(strings.NewReader(raw))); err == nil {
		t.Fatal("failed commit accepted")
	}
	f.s.db.beforeCommit = nil
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.s.Accept(canceled, newID(t), f.envelope(t), io.NopCloser(strings.NewReader(raw))); err == nil {
		t.Fatal("canceled stream accepted")
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_header_metadata`) == 0 && len(f.s.headerCaptures) == 0 &&
			len(f.s.headerParses) == 0,
		"failed stream leaked metadata/permits",
	)
}

type headerFinalRead struct {
	raw []byte
	err error
}

func (r *headerFinalRead) Read(p []byte) (int, error) {
	n := copy(p, r.raw)
	r.raw = r.raw[n:]
	if len(r.raw) == 0 {
		return n, r.err
	}
	return n, nil
}
func (*headerFinalRead) Close() error { return nil }

func TestHeaderCaptureFinalReadAndInterruptedStream(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	raw := "Subject: final\r\n\r\nprivate body"
	r, err := f.s.Accept(ctx, newID(t), f.envelope(t), &headerFinalRead{raw: []byte(raw), err: io.EOF})
	requireReceiptCondition(t, err == nil, "n>0 plus EOF", err)
	if header, _, found, err := f.s.loadHeaderMetadata(
		ctx,
		f.org,
		r.ID,
	); err != nil || !found ||
		string(header) != "Subject: final\r\n\r\n" {
		t.Fatal(string(header), found, err)
	}
	if _, err = f.s.Accept(
		ctx,
		newID(t),
		f.envelope(t),
		&headerFinalRead{raw: []byte(raw), err: io.ErrUnexpectedEOF},
	); err == nil {
		t.Fatal("partial source accepted")
	}
	if _, err = f.s.Accept(
		ctx,
		newID(t),
		f.envelope(t),
		&headerFinalRead{raw: []byte(raw), err: errors.Join(io.ErrUnexpectedEOF, io.EOF)},
	); err == nil {
		t.Fatal("joined failure plus EOF accepted")
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_receipt`) == 1,
		"failed stream published receipt",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_header_metadata`) == 1 && len(f.s.headerCaptures) == 0,
		"partial source persisted or leaked permit",
	)
	streamCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	source := newGated()
	done := make(chan error, 1)
	env, attempt := f.envelope(t), newID(t)
	go func() { _, err := f.s.Accept(streamCtx, attempt, env, source); done <- err }()
	wait(t, source.started)
	requireReceiptCondition(t, len(f.s.headerCaptures) == 1, "capture permit not held during stream")
	cancel()
	select {
	case err = <-done:
		requireReceiptCondition(t, err != nil, "canceled stream accepted")
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop stream")
	}
	requireReceiptCondition(
		t,
		len(f.s.headerCaptures) == 0 && len(f.s.headerParses) == 0,
		"canceled stream leaked permit",
	)
}

func TestHeaderCaptureRetryCannotBindDifferentSourceToDurableCopies(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	env, attempt := f.envelope(t), newID(t)
	original := "Subject: original\r\n\r\noriginal body"
	f.s.afterCopies = func() error { return io.ErrUnexpectedEOF }
	if _, err := f.s.Accept(ctx, attempt, env, io.NopCloser(strings.NewReader(original))); err == nil {
		t.Fatal("pre-finalize interruption accepted")
	}
	f.s.afterCopies = nil
	expire(t, f, attempt)
	// The existing members consume a retry for the same immutable byte record.
	// Even if they return that record's locator, a different streamed header must
	// never become metadata attached to it.
	if _, err := f.s.Accept(
		ctx,
		attempt,
		env,
		io.NopCloser(strings.NewReader("Subject: replaced\r\n\r\ndifferent body")),
	); err == nil {
		t.Fatal("different retry stream accepted")
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_receipt`) == 0 &&
			scalar(t, f.pool, `SELECT count(*) FROM fry_header_metadata`) == 0,
		"retry contaminated metadata",
	)
	expire(t, f, attempt)
	r := accept(t, f.s, attempt, env, original)
	header, err := f.s.Header(ctx, f.org, r.ID)
	if err != nil || string(header) != "Subject: original\r\n\r\n" {
		t.Fatal(string(header), err)
	}
}

func TestHeaderCaptureLostAcceptanceReplyKeepsAtomicMetadata(t *testing.T) {
	f := setup(t)
	env, attempt := f.envelope(t), newID(t)
	f.s.db.afterCommit = func() error { return io.ErrUnexpectedEOF }
	if _, err := f.s.Accept(
		context.Background(),
		attempt,
		env,
		io.NopCloser(strings.NewReader("Subject: committed\r\n\r\nbody")),
	); !errors.Is(
		err,
		ErrUncertain,
	) {
		t.Fatal(err)
	}
	f.s.db.afterCommit = nil
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_receipt`,
		) == 1 && scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_header_metadata`,
		) == 1 &&
			len(f.s.headerCaptures) == 0,
		"lost reply split atomic receipt/header or leaked permit",
	)
	replacement := &headerUnreadSource{}
	if _, err := f.s.Accept(context.Background(), attempt, env, replacement); err != nil || replacement.read {
		t.Fatal(err, replacement.read)
	}
}

func TestHeaderCaptureInsertFailureRollsBackAcceptance(t *testing.T) {
	f := setup(t)
	execSQL(
		t,
		f.pool,
		`CREATE FUNCTION reject_captured_header() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN `+
			`RAISE EXCEPTION 'synthetic capture failure'; END $$; CREATE TRIGGER `+
			`reject_captured_header BEFORE INSERT ON fry_header_metadata FOR EACH ROW EXECUTE `+
			`FUNCTION reject_captured_header()`,
	)
	if _, err := f.s.Accept(
		context.Background(),
		newID(t),
		f.envelope(t),
		io.NopCloser(strings.NewReader("Subject: atomic\r\n\r\nbody")),
	); err == nil {
		t.Fatal("metadata SQL error hidden during acceptance")
	}
	for _, table := range []string{
		"fry_receipt",
		"fry_header_metadata",
		"fry_reference",
		"fry_work",
		"fry_destination",
	} {
		requireReceiptCondition(
			t,
			scalar(t, f.pool, `SELECT count(*) FROM `+table) == 0,
			"partial acceptance escaped rollback",
			table,
		)
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE locator IS NOT NULL`,
		) == 2 && len(
			f.s.headerCaptures,
		) == 0 &&
			len(f.s.headerParses) == 0,
		"metadata failure changed durable copy debt or leaked permit",
	)
}
