//go:build linux

package receiptstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

func outboundAddress(address string) bool {
	if len(address) > 254 || strings.ContainsAny(address, "\r\n\x00") {
		return false
	}
	for _, c := range address {
		if c > 127 {
			return false
		}
	}
	parsed, err := mail.ParseAddress(address)
	return err == nil && parsed.Name == "" && parsed.Address == address && strings.Contains(address, "@")
}

func normalizeOutbound(e Envelope) (Envelope, []byte, error) {
	o := *e.Outbound
	e.Outbound = &o
	if !validOutboundEnvelope(e, o) {
		return e, nil, ErrConflict
	}
	if e.MailFrom != "" && !outboundAddress(e.MailFrom) {
		return e, nil, ErrConflict
	}
	o.Recipients = append([]string(nil), o.Recipients...)
	sort.Strings(o.Recipients)
	unique := o.Recipients[:0]
	for _, recipient := range o.Recipients {
		if !outboundAddress(recipient) {
			return e, nil, ErrConflict
		}
		if len(unique) == 0 || unique[len(unique)-1] != recipient {
			unique = append(unique, recipient)
		}
	}
	o.Recipients = unique
	b, err := json.Marshal(e)
	if len(b) > 64*1024 {
		return e, nil, ErrConflict
	}
	return e, b, err
}

func publishOutbound(ctx context.Context, tx pgx.Tx, receipt ID, envelope Envelope, received time.Time) error {
	if envelope.Outbound == nil {
		return nil
	}
	for _, recipient := range envelope.Outbound.Recipients {
		id, err := hotstore.NewID()
		if err != nil {
			return err
		}
		if _, err = dbgen.New(tx).
			PublishOutboundInsertOutbound(ctx, dbgen.PublishOutboundInsertOutboundParams{
				ID:        id.String(),
				Receipt:   receipt.String(),
				Relay:     envelope.Outbound.Relay.String(),
				Recipient: recipient,
				Deadline:  received.Add(envelope.Outbound.RetryWindow),
			}); err != nil {
			return err
		}
		if _, err = dbgen.New(tx).
			PublishOutboundInsertReference(ctx, dbgen.PublishOutboundInsertReferenceParams{
				Receipt:      receipt.String(),
				ReferenceKey: "outbound:" + id.String(),
			}); err != nil {
			return err
		}
	}
	return nil
}

type OutboundClaim struct {
	ID, Receipt, Organization, Relay ID
	Sender, Recipient                string
	Size                             int64
	Epoch, Attempts                  int64
	Deadline, LeaseUntil             time.Time
}

// ClaimOutbound reserves one recipient handoff. One-recipient relay transactions
// simplify partial rejection in the prototype; batching is future throughput work.
func (s *Store) ClaimOutbound(ctx context.Context, lease time.Duration) (OutboundClaim, error) {
	var result OutboundClaim
	if lease < time.Second || lease > 5*time.Minute {
		return result, ErrConflict
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return result, err
	}
	defer rollback(ctx, tx)
	row, err := dbgen.New(tx).StoreClaimOutboundReadOutbound(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	result, err = decodeOutboundClaim(row)
	if err != nil {
		return result, err
	}
	err = func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			StoreClaimOutboundUpdateOutbound(ctx, dbgen.StoreClaimOutboundUpdateOutboundParams{
				ID:      row.ID,
				Column2: lease.Microseconds(),
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		result.Epoch = sqlcResult.Epoch
		result.Attempts = sqlcResult.Attempts
		if sqlcResult.LeaseUntil == nil {
			return fmt.Errorf("receiptstore: unexpected NULL in StoreClaimOutboundUpdateOutbound.LeaseUntil")
		}
		result.LeaseUntil = *sqlcResult.LeaseUntil
		return nil
	}()
	if err != nil {
		return result, err
	}
	return result, commit(ctx, tx)
}

type OutboundResult struct {
	// State is retry, handed-off, or failed. Handoff does not mean recipient delivery.
	State        string
	ResponseCode int
	Diagnostic   string
	RetryAfter   time.Duration
}

// CompleteOutbound fences stale workers and persists terminal state, failure
// notice intent, and reference release together. A lost relay reply is retried;
// neither SMTP nor this ledger promises exactly-once delivery at the relay.
func (s *Store) CompleteOutbound(ctx context.Context, claim OutboundClaim, result OutboundResult) error {
	if err := validateOutboundResult(result); err != nil {
		return err
	}
	if result.ResponseCode < 0 || result.ResponseCode > 599 {
		return ErrConflict
	}
	code := int32(result.ResponseCode)
	tx, err := s.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if _, err = lockReceipt(ctx, tx, claim.Organization, claim.Receipt); err != nil {
		return err
	}
	row, err := dbgen.New(tx).StoreCompleteOutboundReadOutbound(ctx, claim.ID.String())
	if err != nil {
		return err
	}
	if err = guardOutboundClaim(row, claim); err != nil {
		return err
	}
	return s.completeOutboundTx(ctx, tx, claim, result, code, row.Deadline, row.Column8)
}

func guardOutboundClaim(row dbgen.StoreCompleteOutboundReadOutboundRow, claim OutboundClaim) error {
	if row.Receipt != claim.Receipt.String() || row.Relay != claim.Relay.String() || row.Recipient != claim.Recipient {
		return ErrConflict
	}
	if row.State != "queued" || row.Epoch != claim.Epoch || row.LeaseUntil == nil ||
		!row.LeaseUntil.After(row.Column8) {
		return ErrStale
	}
	return nil
}

func (s *Store) completeOutboundTx(
	ctx context.Context,
	tx pgx.Tx,
	claim OutboundClaim,
	result OutboundResult,
	code int32,
	deadline, now time.Time,
) error {
	var err error
	if result.State == "retry" && !deadline.After(now) {
		result.State = "failed"
		code = 554
		result.Diagnostic = "Outbound retry period expired"
	}
	if result.State == "retry" {
		return retryOutboundTx(ctx, tx, claim.ID, result, code)
	}
	if _, err = dbgen.New(tx).
		StoreCompleteOutboundUpdateOutbound2(ctx, dbgen.StoreCompleteOutboundUpdateOutbound2Params{
			ID:           claim.ID.String(),
			State:        result.State,
			ResponseCode: sqlcPointer(code),
			Diagnostic:   sqlcPointer(result.Diagnostic),
		}); err != nil {
		return err
	}
	if result.State == "failed" {
		if err = queueOutboundFailure(ctx, tx, claim, result, code); err != nil {
			return err
		}
	}
	if err = releaseMailboxRef(ctx, tx, claim.Receipt, "delivery", "outbound:"+claim.ID.String()); err != nil {
		return err
	}
	if _, err = s.reconcileLifetime(ctx, tx, claim.Receipt); err != nil {
		return err
	}
	return commit(ctx, tx)
}

func (s *Store) OutboundState(ctx context.Context, org, receipt ID) (map[string]string, error) {
	rows, err := s.db.pool.Query(ctx, dbgen.StoreOutboundState, org.String(), receipt.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var recipient, state string
		if err = rows.Scan(&recipient, &state); err != nil {
			return nil, err
		}
		result[recipient] = state
	}
	return result, rows.Err()
}

func validOutboundEnvelope(e Envelope, o Outbound) bool {
	return e.Organization != (ID{}) && len(e.Destinations) == 0 && e.Source == nil && validOutboundRoute(o)
}

func validOutboundRoute(o Outbound) bool {
	return o.Relay != (ID{}) && len(o.Recipients) > 0 && len(o.Recipients) <= 1024 && o.RetryWindow >= time.Second &&
		o.RetryWindow <= 30*24*time.Hour &&
		o.RetryWindow%time.Second == 0
}

func decodeOutboundClaim(row dbgen.StoreClaimOutboundReadOutboundRow) (OutboundClaim, error) {
	result := OutboundClaim{Recipient: row.Recipient, Size: row.RawSize, Deadline: row.Deadline}
	var err error
	if result.ID, err = idFrom(row.ID); err != nil {
		return result, err
	}
	if result.Receipt, err = idFrom(row.Receipt); err != nil {
		return result, err
	}
	if result.Organization, err = idFrom(row.Organization); err != nil {
		return result, err
	}
	if result.Relay, err = idFrom(row.Relay); err != nil {
		return result, err
	}
	var envelope Envelope
	if err = json.Unmarshal(row.Envelope, &envelope); err != nil {
		return result, err
	}
	result.Sender = envelope.MailFrom
	return result, nil
}

func validateOutboundResult(result OutboundResult) error {
	if (result.State != "retry" && result.State != "handed-off" && result.State != "failed") ||
		len(result.Diagnostic) > 1024 ||
		result.RetryAfter < 0 ||
		result.RetryAfter > 24*time.Hour {
		return ErrConflict
	}
	if result.State == "handed-off" && (result.ResponseCode < 200 || result.ResponseCode > 299) {
		return ErrConflict
	}
	return nil
}

func queueOutboundFailure(
	ctx context.Context,
	tx pgx.Tx,
	claim OutboundClaim,
	result OutboundResult,
	code int32,
) error {
	// Read the immutable sender from authority instead of trusting a caller's copy.
	var sender string
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).StoreCompleteOutboundReadReceipt(ctx, claim.Receipt.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		sender = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	if sender != "" {
		id, e := hotstore.NewID()
		if e != nil {
			return e
		}
		if _, err := dbgen.New(tx).
			StoreCompleteOutboundInsertFailureNotice(ctx, dbgen.StoreCompleteOutboundInsertFailureNoticeParams{
				ID:              id.String(),
				Job:             claim.ID.String(),
				Organization:    claim.Organization.String(),
				Relay:           claim.Relay.String(),
				Recipient:       sender,
				FailedRecipient: claim.Recipient,
				ResponseCode:    code,
				Diagnostic:      result.Diagnostic,
			}); err != nil {
			return err
		}
	}
	return nil
}

func retryOutboundTx(ctx context.Context, tx pgx.Tx, id ID, result OutboundResult, code int32) error {
	delay := result.RetryAfter
	if delay < time.Second {
		delay = time.Second
	}
	_, err := dbgen.New(tx).
		StoreCompleteOutboundUpdateOutbound(ctx, dbgen.StoreCompleteOutboundUpdateOutboundParams{
			ID:           id.String(),
			Column2:      delay.Microseconds(),
			ResponseCode: sqlcPointer(code),
			Diagnostic:   sqlcPointer(result.Diagnostic),
		})
	if err != nil {
		return err
	}
	return commit(ctx, tx)
}
