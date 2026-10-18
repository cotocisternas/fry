//go:build linux

package admin

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/cotocisternas/fry/internal/admin/dbgen"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/jackc/pgx/v5"
)

// TXTResolver is the only external verification dependency. A nil Service
// resolver uses the system resolver. Tests set it before concurrent use.
type TXTResolver interface {
	LookupTXT(context.Context, string) ([]string, error)
}

// SenderChallenge expires after 24 hours. Exact-address scopes demonstrate
// control of that address domain's verification DNS, not a mailbox user identity.
type SenderChallenge struct {
	ID           string    `json:"id"`
	Organization string    `json:"organization"`
	Scope        string    `json:"scope"`
	DNSName      string    `json:"dns_name"`
	TXTValue     string    `json:"txt_value"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Sender grants are a point-in-time DNS observation or an explicit operator
// override. They remain valid until revoked; DNS is not continuously polled.
type SenderGrant struct {
	ID           string     `json:"id"`
	Organization string     `json:"organization"`
	Scope        string     `json:"scope"`
	Version      int64      `json:"version"`
	Active       bool       `json:"active"`
	Evidence     string     `json:"evidence"`
	Actor        string     `json:"actor"`
	Reason       string     `json:"reason"`
	VerifiedAt   *time.Time `json:"verified_at"`
}

func senderGrant(v receiptstore.SenderGrant) SenderGrant {
	return SenderGrant{
		v.ID.String(),
		v.Organization.String(),
		v.Scope,
		v.Version,
		v.Active,
		v.Evidence,
		v.Actor,
		v.Reason,
		v.VerifiedAt,
	}
}

func senderActor(ctx context.Context, tx pgx.Tx, token, org string, global bool) (Identity, error) {
	who, err := authorize(ctx, tx, token)
	if err != nil {
		return who, err
	}
	scope := org
	if global {
		scope = "*"
	}
	if !allowed(who, scope, false) {
		return who, ErrForbidden
	}
	var exists bool
	if exists, err = dbgen.New(tx).OrganizationExistsForSender(ctx, org); err != nil {
		return who, err
	}
	if !exists {
		return who, ErrInvalid
	}
	return who, nil
}

func (s *Service) SenderGrants(ctx context.Context, token, org string) ([]SenderGrant, error) {
	oid, err := mailID(org)
	if err != nil {
		return nil, err
	}
	tx, err := s.begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	if _, err = senderActor(ctx, tx, token, org, false); err != nil {
		return nil, err
	}
	values, err := receiptstore.ListSenderGrants(ctx, tx, oid)
	if err != nil {
		return nil, err
	}
	result := make([]SenderGrant, 0, len(values))
	for _, v := range values {
		result = append(result, senderGrant(v))
	}
	return result, nil
}

func (s *Service) SenderChallenge(ctx context.Context, token, org, scope string) (SenderChallenge, error) {
	var result SenderChallenge
	oid, err := mailID(org)
	if err != nil {
		return result, err
	}
	scope, err = receiptstore.CanonicalSenderScope(scope)
	if err != nil {
		return result, ErrInvalid
	}
	tx, err := s.begin(ctx, false)
	if err != nil {
		return result, err
	}
	defer rollback(ctx, tx)
	who, err := senderActor(ctx, tx, token, org, false)
	if err != nil {
		return result, err
	}
	v, err := receiptstore.CreateSenderChallenge(ctx, tx, oid, scope)
	if err != nil {
		return result, err
	}
	if err = audit(ctx, tx, who.ID, "sender-challenge-create", org, v.ID.String()); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	return SenderChallenge{v.ID.String(), org, v.Scope, v.DNSName, v.TXTValue, v.ExpiresAt}, nil
}

func (s *Service) VerifySender(ctx context.Context, token, org, challenge string) (SenderGrant, error) {
	var result SenderGrant
	oid, err := mailID(org)
	if err != nil {
		return result, err
	}
	cid, err := mailID(challenge)
	if err != nil {
		return result, err
	}
	pending, err := s.authorizedSenderChallenge(ctx, token, org, oid, cid)
	if err != nil {
		return result, err
	}
	records, err := s.senderRecords(ctx, pending.DNSName)
	if err != nil {
		return result, err
	}
	tx, err := s.begin(ctx, false)
	if err != nil {
		return result, err
	}
	defer rollback(ctx, tx)
	who, err := senderActor(ctx, tx, token, org, false)
	if err != nil {
		return result, err
	}
	grant, err := receiptstore.CompleteSenderChallenge(ctx, tx, oid, cid, records, who.ID)
	if err != nil {
		return result, err
	}
	if err = audit(ctx, tx, who.ID, "sender-dns-verify", org, grant.ID.String()); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	return senderGrant(grant), nil
}

func senderReason(reason string, required bool) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" && !required {
		return "Administrator revoked sender grant", nil
	}
	if reason == "" || len(reason) > 1024 || strings.ContainsAny(reason, "\x00\r\n") {
		return "", ErrInvalid
	}
	return reason, nil
}

func (s *Service) RevokeSender(ctx context.Context, token, org, scope, reason string) error {
	oid, err := mailID(org)
	if err != nil {
		return err
	}
	reason, err = senderReason(reason, false)
	if err != nil {
		return err
	}
	scope, err = receiptstore.CanonicalSenderScope(scope)
	if err != nil {
		return ErrInvalid
	}
	tx, err := s.begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	who, err := senderActor(ctx, tx, token, org, false)
	if err != nil {
		return err
	}
	if err = receiptstore.RevokeSenderGrant(ctx, tx, oid, scope, who.ID, reason); err != nil {
		return err
	}
	if err = audit(ctx, tx, who.ID, "sender-grant-revoke", org, scope); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) OverrideSender(ctx context.Context, token, org, scope, reason string) (SenderGrant, error) {
	var result SenderGrant
	oid, err := mailID(org)
	if err != nil {
		return result, err
	}
	reason, err = senderReason(reason, true)
	if err != nil {
		return result, err
	}
	scope, err = receiptstore.CanonicalSenderScope(scope)
	if err != nil {
		return result, ErrInvalid
	}
	tx, err := s.begin(ctx, false)
	if err != nil {
		return result, err
	}
	defer rollback(ctx, tx)
	who, err := senderActor(ctx, tx, token, org, true)
	if err != nil {
		return result, err
	}
	grant, err := receiptstore.OverrideSenderGrant(ctx, tx, oid, scope, who.ID, reason)
	if err != nil {
		return result, err
	}
	if err = audit(ctx, tx, who.ID, "sender-grant-override", org, grant.ID.String()); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	return senderGrant(grant), nil
}

func (s *Service) authorizedSenderChallenge(
	ctx context.Context,
	token, org string,
	oid, cid receiptstore.ID,
) (receiptstore.SenderChallenge, error) {
	// Snapshot only an authorized live challenge. No transaction or policy lock
	// remains held across external DNS, and completion rechecks current authority.
	tx, err := s.begin(ctx, false)
	if err != nil {
		return receiptstore.SenderChallenge{}, err
	}
	if _, err = senderActor(ctx, tx, token, org, false); err != nil {
		rollback(ctx, tx)
		return receiptstore.SenderChallenge{}, err
	}
	pending, err := receiptstore.LookupSenderChallenge(ctx, tx, oid, cid)
	rollback(ctx, tx)
	if err != nil {
		return receiptstore.SenderChallenge{}, err
	}
	return pending, nil
}

func (s *Service) senderRecords(ctx context.Context, name string) ([]string, error) {
	resolver := s.senderDNS
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	records, err := resolver.LookupTXT(lookupCtx, name)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("admin: sender DNS lookup failed")
	}
	size := 0
	for _, v := range records {
		size += len(v)
	}
	if len(records) > 1024 || size > 64<<10 {
		return nil, fmt.Errorf("admin: sender DNS response exceeds limits")
	}
	return records, nil
}
