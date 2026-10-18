//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func relayDestination(t *testing.T) RelayDestination {
	t.Helper()
	return RelayDestination{
		ID:        newID(t),
		Address:   "Relay.EXAMPLE.test:00587",
		Transport: "starttls",
		Username:  "exact-account",
	}
}

func TestRelayDestinationCanonicalAddress(t *testing.T) {
	destination := relayDestination(t)
	canonical, err := destination.Canonical()
	requireReceiptCondition(
		t,
		err == nil && canonical.Address == "relay.example.test:587" && canonical.Username == destination.Username,
		canonical,
		err,
	)
	for _, address := range []string{
		"host:0",
		"host:65536",
		"host: smtp",
		" host:587",
		"host:587 ",
		"host:submission",
		"host",
	} {
		invalid := destination
		invalid.Address = address
		if _, err := invalid.Canonical(); err == nil {
			t.Fatal("invalid relay accepted", address)
		}
	}
}

func TestRelayBindingPreservesAcceptedDestinationAndAtomicCatalog(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	destination := relayDestination(t)
	requireReceiptNoError(t, f.s.BindRelayDestinations(ctx, []RelayDestination{destination}))
	envelope := outbound(t, f, "recipient@example.test")
	envelope.Outbound.Relay = destination.ID
	receipt := accept(t, f.s, newID(t), envelope, "durable mail")
	// A restarted worker's new destination must not reinterpret the accepted ID.
	for _, change := range []func(*RelayDestination){
		func(r *RelayDestination) { r.Address = "different.example.test:587" },
		func(r *RelayDestination) { r.Transport = "tls" },
		func(r *RelayDestination) { r.Username = "another-account" },
	} {
		changed := destination
		change(&changed)
		newDestination := relayDestination(t)
		if err := f.s.BindRelayDestinations(
			ctx,
			[]RelayDestination{newDestination, changed},
		); !errors.Is(
			err,
			ErrRelayBinding,
		) {
			t.Fatal("accepted destination changed", err)
		}
		requireReceiptCondition(
			t,
			scalar(t, f.pool, `SELECT count(*) FROM fry_relay_binding WHERE id=$1`, newDestination.ID.String()) == 0,
			"partial catalog committed",
		)
	}
	same := destination
	same.Address = "relay.example.test:587"
	if err := f.s.BindRelayDestinations(ctx, []RelayDestination{same}); err != nil {
		t.Fatal("same canonical destination rejected", err)
	}
	claim, err := f.s.ClaimOutbound(ctx, time.Minute)
	requireReceiptCondition(t, err == nil && claim.Relay == destination.ID && claim.Receipt == receipt.ID, claim, err)
}

func TestRelayBindingRejectsHistoricalAdoptionAndMissingPendingDestinations(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	legacy := relayDestination(t)
	envelope := outbound(t, f, "legacy@example.test")
	envelope.Outbound.Relay = legacy.ID
	accept(t, f.s, newID(t), envelope, "unbound legacy request")
	if err := f.s.BindRelayDestinations(ctx, []RelayDestination{legacy}); !errors.Is(err, ErrRelayBinding) {
		t.Fatal("legacy queue silently adopted", err)
	}
	// No mutable setting can fill in the unknown historical endpoint.
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_relay_binding`) == 0,
		"failed adoption persisted",
	)
}

func TestRelayBindingRequiresQueuedAndFailureNoticeDestinations(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	destination := relayDestination(t)
	requireReceiptNoError(t, f.s.BindRelayDestinations(ctx, []RelayDestination{destination}))
	envelope := outbound(t, f, "recipient@example.test")
	envelope.Outbound.Relay = destination.ID
	accept(t, f.s, newID(t), envelope, "notice must retain original relay")
	other := relayDestination(t)
	if err := f.s.BindRelayDestinations(ctx, []RelayDestination{other}); !errors.Is(err, ErrRelayBinding) {
		t.Fatal("queued relay omission accepted", err)
	}
	claim, err := f.s.ClaimOutbound(ctx, time.Minute)
	requireReceiptNoError(t, err)
	err = f.s.CompleteOutbound(
		ctx,
		claim,
		OutboundResult{State: "failed", ResponseCode: 550, Diagnostic: "test rejection"},
	)
	requireReceiptNoError(t, err)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_outbound WHERE state='queued'`) == 0 &&
			scalar(t, f.pool, `SELECT count(*) FROM fry_failure_notice WHERE state='pending'`) == 1,
		"notice setup failed",
	)
	if err = f.s.BindRelayDestinations(ctx, []RelayDestination{other}); !errors.Is(err, ErrRelayBinding) {
		t.Fatal("pending notice relay omission accepted", err)
	}
	err = f.s.BindRelayDestinations(ctx, []RelayDestination{destination, other})
	requireReceiptNoError(t, err)
}

func TestRelayBindingConcurrentFirstDestinationIsImmutable(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a := relayDestination(t)
	b := a
	b.Address = "different.example.test:587"
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, destination := range []RelayDestination{a, b} {
		group.Add(1)
		go func(r RelayDestination) {
			defer group.Done()
			<-start
			results <- f.s.BindRelayDestinations(ctx, []RelayDestination{r})
		}(destination)
	}
	close(start)
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			requireReceiptCondition(t, errors.Is(err, ErrRelayBinding), err)
		}
	}
	requireReceiptCondition(
		t,
		successes == 1 && scalar(t, f.pool, `SELECT count(*) FROM fry_relay_binding`) == 1,
		"racing first bindings disagreed",
		successes,
	)
}

func TestHasPendingOutboundAllowsOnlyQuiescentDisable(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	check := func(want bool) {
		t.Helper()
		got, err := f.s.HasPendingOutbound(ctx)
		requireReceiptConditionf(t, err == nil && got == want, "pending=%v want %v: %v", got, want, err)
	}
	check(false)
	// A live acceptance lease may still publish an outbound job after startup.
	env := outbound(t, f, "recipient@example.test")
	attempt := newID(t)
	prepare(t, f, attempt, env)
	check(true)
	if _, err := f.pool.Exec(
		ctx,
		`UPDATE fry_attempt SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`,
		attempt.String(),
	); err != nil {
		t.Fatal(err)
	}
	check(false)
	accept(t, f.s, newID(t), env, "queued mail")
	check(true)
	claim, err := f.s.ClaimOutbound(ctx, time.Minute)
	requireReceiptNoError(t, err)
	err = f.s.CompleteOutbound(
		ctx,
		claim,
		OutboundResult{State: "handed-off", ResponseCode: 250, Diagnostic: "accepted"},
	)
	requireReceiptNoError(t, err)
	// Terminal history and expired unaccepted attempts do not permanently force
	// outbound to remain enabled after all obligations have resolved.
	check(false)
	accept(t, f.s, newID(t), env, "will need a failure notice")
	claim, err = f.s.ClaimOutbound(ctx, time.Minute)
	requireReceiptNoError(t, err)
	err = f.s.CompleteOutbound(ctx, claim, OutboundResult{State: "failed", ResponseCode: 550, Diagnostic: "rejected"})
	requireReceiptNoError(t, err)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_outbound WHERE state='queued'`) == 0,
		"recipient still queued",
	)
	check(true)
}
