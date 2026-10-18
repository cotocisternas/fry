//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func outbound(t *testing.T, f *fixture, recipients ...string) Envelope {
	t.Helper()
	return Envelope{
		Organization: f.org,
		MailFrom:     "sender@example.test",
		Outbound:     &Outbound{Relay: newID(t), Recipients: recipients, RetryWindow: time.Hour},
	}
}

func TestOutboundAcceptanceAndIndependentRecipients(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	env := outbound(t, f, "a@example.net", "b@example.net", "a@example.net")
	attempt := newID(t)
	r := accept(t, f.s, attempt, env, "raw outgoing")
	again := accept(t, f.s, attempt, env, "must not be read")
	requireReceiptCondition(t, again.ID == r.ID, "receipt changed")
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_outbound`) == 2 &&
			scalar(t, f.pool, `SELECT count(*) FROM fry_work`) == 0 &&
			scalar(t, f.pool, `SELECT count(*) FROM fry_destination`) == 0,
		"outbound publication incorrect",
	)
	a, err := f.s.ClaimOutbound(ctx, time.Minute)
	requireReceiptNoError(t, err)
	b, err := f.s.ClaimOutbound(ctx, time.Minute)
	requireReceiptCondition(t, err == nil && a.ID != b.ID, a, b, err)
	if _, err = f.s.ClaimOutbound(ctx, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatal("active handoff claimed twice", err)
	}
	execSQL(t, f.pool, `UPDATE fry_reference SET expires_at=clock_timestamp()-interval '1 day' WHERE `+
		`kind='recovery'`)
	err = f.s.CompleteOutbound(
		ctx,
		a,
		OutboundResult{State: "handed-off", ResponseCode: 250, Diagnostic: "Accepted by selected relay"},
	)
	requireReceiptNoError(t, err)
	status, err := f.s.CleanupStatus(ctx, f.org, r.ID)
	requireReceiptCondition(t, err == nil && status.State == "live", "remaining recipient lost bytes", status, err)
	err = f.s.CompleteOutbound(
		ctx,
		b,
		OutboundResult{State: "failed", ResponseCode: 550, Diagnostic: "Recipient rejected"},
	)
	requireReceiptNoError(t, err)
	status, err = f.s.CleanupStatus(ctx, f.org, r.ID)
	requireReceiptCondition(t, err == nil && status.State == "retiring", status, err)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_failure_notice WHERE recipient='sender@example.test'`) == 1,
		"failure notice intent missing",
	)
	if err = f.s.CompleteOutbound(
		ctx,
		b,
		OutboundResult{State: "failed", ResponseCode: 550},
	); !errors.Is(
		err,
		ErrStale,
	) {
		t.Fatal("terminal transition replayed", err)
	}
	requireReceiptCondition(t, scalar(t, f.pool, `SELECT count(*) FROM fry_failure_notice`) == 1, "duplicate notice")
}

func TestOutboundRetryFencingAndBoundedFailure(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	env := outbound(t, f, "a@example.net")
	accept(t, f.s, newID(t), env, "raw")
	old, err := f.s.ClaimOutbound(ctx, time.Minute)
	requireReceiptNoError(t, err)
	err = f.s.CompleteOutbound(ctx, old, OutboundResult{State: "retry", ResponseCode: 451, RetryAfter: time.Minute})
	requireReceiptNoError(t, err)
	if _, err = f.s.ClaimOutbound(ctx, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatal("retry backoff ignored", err)
	}
	execSQL(t, f.pool, `UPDATE fry_outbound SET available_at=clock_timestamp()-interval '1 second'`)
	next, err := f.s.ClaimOutbound(ctx, time.Minute)
	requireReceiptCondition(t, err == nil && next.Epoch > old.Epoch, next, err)
	if err = f.s.CompleteOutbound(
		ctx,
		old,
		OutboundResult{State: "handed-off", ResponseCode: 250},
	); !errors.Is(
		err,
		ErrStale,
	) {
		t.Fatal("old worker changed queue", err)
	}
	execSQL(t, f.pool, `UPDATE fry_outbound SET deadline=clock_timestamp()-interval '1 second'`)
	err = f.s.CompleteOutbound(ctx, next, OutboundResult{State: "retry", ResponseCode: 451})
	requireReceiptNoError(t, err)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_outbound WHERE state='failed'`) == 1,
		"retry deadline ignored",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_failure_notice`) == 1,
		"deadline failure not recorded",
	)
}

func TestOutboundNullSenderDoesNotBounceAndDurationRequired(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	env := outbound(t, f, "recipient@example.net")
	env.MailFrom = ""
	accept(t, f.s, newID(t), env, "failure notice")
	claim, err := f.s.ClaimOutbound(ctx, time.Minute)
	requireReceiptNoError(t, err)
	err = f.s.CompleteOutbound(ctx, claim, OutboundResult{State: "failed", ResponseCode: 550})
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, scalar(t, f.pool, `SELECT count(*) FROM fry_failure_notice`) == 0, "bounce loop created")
	env.Outbound.RetryWindow = 0
	if _, _, err = normalize(env); err == nil {
		t.Fatal("silent retry default selected")
	}
}

func TestFailureNoticeRenderedOnceAndCannotBounceAgain(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	accept(t, f.s, newID(t), outbound(t, f, "unavailable@example.net"), "original")
	claim, err := f.s.ClaimOutbound(ctx, time.Minute)
	requireReceiptNoError(t, err)
	err = f.s.CompleteOutbound(
		ctx,
		claim,
		OutboundResult{State: "failed", ResponseCode: 550, Diagnostic: "Recipient rejected"},
	)
	requireReceiptNoError(t, err)
	n, err := f.s.QueueFailureNotices(ctx, "fry.local", time.Hour, 10)
	requireReceiptCondition(t, err == nil && n == 1, n, err)
	if n, err = f.s.QueueFailureNotices(ctx, "changed.local", 2*time.Hour, 10); err != nil || n != 0 {
		t.Fatal("queued notice replayed", n, err)
	}
	notice, err := f.s.ClaimOutbound(ctx, time.Minute)
	requireReceiptCondition(
		t,
		err == nil && notice.Sender == "" && notice.Recipient == "sender@example.test" && notice.Relay == claim.Relay,
		notice,
		err,
	)
	var body strings.Builder
	err = f.s.Read(ctx, f.org, notice.Receipt, 0, notice.Size, &body)
	requireReceiptNoError(t, err)
	if !strings.Contains(body.String(), "Content-Type: message/delivery-status") ||
		!strings.Contains(body.String(), "Final-Recipient: rfc822; unavailable@example.net\r\nAction: failed") {
		t.Fatal(body.String())
	}
	err = f.s.CompleteOutbound(ctx, notice, OutboundResult{State: "failed", ResponseCode: 550})
	requireReceiptNoError(t, err)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_failure_notice`) == 1,
		"recursive failure notice",
	)
	// Simulate a lost final mark after notice acceptance; its saved request and
	// attempt resolve the original receipt even when render settings changed.
	execSQL(t, f.pool, `UPDATE fry_failure_notice SET state='pending',receipt=NULL`)
	n, err = f.s.QueueFailureNotices(ctx, "changed.local", 2*time.Hour, 10)
	requireReceiptCondition(t, err == nil && n == 1, n, err)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_receipt`) == 2,
		"ambiguous notice enqueue duplicated receipt",
	)
}
