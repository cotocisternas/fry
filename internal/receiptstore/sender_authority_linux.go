//go:build linux

package receiptstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

var ErrSenderUnauthorized = errors.New("receiptstore: sender proof is invalid, stale, or revoked")

// ErrSenderAuthorization is retained as the proof-validation spelling.
var ErrSenderAuthorization = ErrSenderUnauthorized

type SenderChallenge struct {
	ID        ID        `json:"id"`
	Scope     string    `json:"scope"`
	DNSName   string    `json:"dns_name"`
	TXTValue  string    `json:"txt_value,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (c SenderChallenge) MarshalJSON() ([]byte, error) {
	type value SenderChallenge
	return json.Marshal(struct {
		ID string `json:"id"`
		value
	}{c.ID.String(), value(c)})
}

type SenderGrant struct {
	ID           ID         `json:"id"`
	Organization ID         `json:"organization"`
	Scope        string     `json:"scope"`
	Version      int64      `json:"version"`
	Active       bool       `json:"active"`
	Evidence     string     `json:"evidence"`
	Actor        string     `json:"actor"`
	Reason       string     `json:"reason"`
	VerifiedAt   *time.Time `json:"verified_at,omitempty"`
}

func (g SenderGrant) MarshalJSON() ([]byte, error) {
	type value SenderGrant
	return json.Marshal(struct {
		ID           string `json:"id"`
		Organization string `json:"organization"`
		value
	}{g.ID.String(), g.Organization.String(), value(g)})
}

// CanonicalSenderScope accepts a domain or exact ASCII address. Local parts are
// case sensitive; domains are not. Incoming-domain ownership is independent.
func CanonicalSenderScope(scope string) (string, error) {
	domain := scope
	if at := strings.LastIndexByte(scope, '@'); at >= 0 {
		if err := validateSenderLocalPart(scope, at); err != nil {
			return "", err
		}
		domain = scope[at+1:]
	}
	domain = strings.ToLower(domain)
	// Leave room for the _fry-verify label within DNS's textual name limit.
	if domain == "" || len(domain) > 241 {
		return "", ErrSenderAuthorization
	}
	if err := validateSenderDomainLabels(domain); err != nil {
		return "", err
	}
	if before, _, ok := strings.CutLast(scope, "@"); ok {
		return before + "@" + domain, nil
	}
	return domain, nil
}

func senderDomain(scope string) string {
	if _, after, ok := strings.CutLast(scope, "@"); ok {
		return after
	}
	return scope
}

func validSenderActor(actor, reason string, requireReason bool) bool {
	return actor != "" && len(actor) <= 256 && !strings.ContainsAny(actor, "\x00\r\n") && len(reason) <= 1024 &&
		!strings.ContainsRune(reason, 0) &&
		(!requireReason || strings.TrimSpace(reason) != "")
}

func ensureSenderGrant(ctx context.Context, tx pgx.Tx, org ID, scope string) (ID, int64, error) {
	if org == (ID{}) {
		return ID{}, 0, ErrSenderAuthorization
	}
	id, err := hotstore.NewID()
	if err != nil {
		return ID{}, 0, err
	}
	if _, err = dbgen.New(tx).
		EnsureSenderGrantInsertSenderGrant(ctx, dbgen.EnsureSenderGrantInsertSenderGrantParams{
			ID:           id.String(),
			Organization: org.String(),
			Scope:        scope,
		}); err != nil {
		return ID{}, 0, err
	}
	var raw string
	var epoch int64
	err = func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			EnsureSenderGrantReadSenderGrant(ctx, dbgen.EnsureSenderGrantReadSenderGrantParams{
				Organization: org.String(),
				Scope:        scope,
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		raw = sqlcResult.ID
		epoch = sqlcResult.ChallengeEpoch
		return nil
	}()
	if err != nil {
		return ID{}, 0, err
	}
	id, err = idFrom(raw)
	return id, epoch, err
}

func invalidateSenderChallenges(ctx context.Context, tx pgx.Tx, grant ID) error {
	_, err := dbgen.New(tx).InvalidateSenderChallenges(ctx, grant.String())
	return err
}

func CreateSenderChallenge(ctx context.Context, tx pgx.Tx, org ID, scope string) (SenderChallenge, error) {
	var result SenderChallenge
	scope, err := CanonicalSenderScope(scope)
	if err != nil {
		return result, err
	}
	grant, epoch, err := ensureSenderGrant(ctx, tx, org, scope)
	if err != nil {
		return result, err
	}
	if err = invalidateSenderChallenges(ctx, tx, grant); err != nil {
		return result, err
	}
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return result, err
	}
	result.ID, err = hotstore.NewID()
	if err != nil {
		return result, err
	}
	result.Scope = scope
	result.DNSName = "_fry-verify." + senderDomain(scope) + "."
	result.TXTValue = "fry-sender-v1=" + hex.EncodeToString(token)
	hash := sha256.Sum256([]byte(result.TXTValue))
	if _, err = dbgen.New(tx).CreateSenderChallengeUpdateSenderGrant(ctx, grant.String()); err != nil {
		return result, err
	}
	err = func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			CreateSenderChallengeInsertSenderChallenge(ctx, dbgen.CreateSenderChallengeInsertSenderChallengeParams{
				ID:        result.ID.String(),
				GrantID:   grant.String(),
				Epoch:     epoch + 1,
				TokenHash: hash[:],
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		result.ExpiresAt = sqlcResult
		return nil
	}()
	return result, err
}

func LookupSenderChallenge(ctx context.Context, tx pgx.Tx, org, id ID) (SenderChallenge, error) {
	result := SenderChallenge{ID: id}
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			LookupSenderChallenge(ctx, dbgen.LookupSenderChallengeParams{ID: id.String(), Organization: org.String()})
		if sqlcErr != nil {
			return sqlcErr
		}
		result.Scope = sqlcResult.Scope
		result.ExpiresAt = sqlcResult.ExpiresAt
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	}
	if err == nil {
		result.DNSName = "_fry-verify." + senderDomain(result.Scope) + "."
	}
	return result, err
}

func CompleteSenderChallenge(
	ctx context.Context,
	tx pgx.Tx,
	org, id ID,
	observedTXT []string,
	actor string,
) (SenderGrant, error) {
	var result SenderGrant
	if !validSenderActor(actor, "", false) || len(observedTXT) > 1024 {
		return result, ErrSenderAuthorization
	}
	raw, err := dbgen.New(tx).
		CompleteSenderChallengeReadSenderChallenge(ctx, dbgen.CompleteSenderChallengeReadSenderChallengeParams{
			ID:           id.String(),
			Organization: org.String(),
		})
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	grant, err := idFrom(raw)
	if err != nil {
		return result, err
	}
	hash, err := dbgen.New(tx).
		CompleteSenderChallengeReadSenderChallengePending(
			ctx,
			dbgen.CompleteSenderChallengeReadSenderChallengePendingParams{
				ID:      id.String(),
				GrantID: raw,
			},
		)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrSenderAuthorization
	}
	if err != nil {
		return result, err
	}
	matched := matchesSenderChallenge(hash, observedTXT)
	if !matched {
		return result, ErrSenderAuthorization
	}
	if _, err = dbgen.New(tx).
		CompleteSenderChallengeUpdateSenderChallenge(ctx, dbgen.CompleteSenderChallengeUpdateSenderChallengeParams{
			ID:          id.String(),
			CompletedBy: new(actor),
		}); err != nil {
		return result, err
	}
	return activateSenderGrant(ctx, tx, grant, actor, "dns-txt", "")
}

func activateSenderGrant(
	ctx context.Context,
	tx pgx.Tx,
	grant ID,
	actor, evidence, reason string,
) (SenderGrant, error) {
	if _, err := dbgen.New(tx).
		ActivateSenderGrant(ctx, dbgen.ActivateSenderGrantParams{
			ID:       grant.String(),
			Actor:    actor,
			Evidence: evidence,
			Reason:   reason,
		}); err != nil {
		return SenderGrant{}, err
	}
	if err := invalidateSenderChallenges(ctx, tx, grant); err != nil {
		return SenderGrant{}, err
	}
	if err := senderGrantEvent(ctx, tx, grant, "enable", actor, evidence, reason); err != nil {
		return SenderGrant{}, err
	}
	return getSenderGrant(ctx, tx, grant)
}

func senderGrantEvent(ctx context.Context, tx pgx.Tx, grant ID, action, actor, evidence, reason string) error {
	_, err := dbgen.New(tx).
		SenderGrantEvent(ctx, dbgen.SenderGrantEventParams{
			ID:       grant.String(),
			Action:   action,
			Actor:    actor,
			Evidence: evidence,
			Reason:   reason,
		})
	return err
}

// OverrideSenderGrant is a privileged primitive; callers must authorize a global
// operator and commit their administrative audit in this same transaction.
func OverrideSenderGrant(ctx context.Context, tx pgx.Tx, org ID, scope, actor, reason string) (SenderGrant, error) {
	if !validSenderActor(actor, reason, true) {
		return SenderGrant{}, ErrSenderAuthorization
	}
	scope, err := CanonicalSenderScope(scope)
	if err != nil {
		return SenderGrant{}, err
	}
	grant, _, err := ensureSenderGrant(ctx, tx, org, scope)
	if err != nil {
		return SenderGrant{}, err
	}
	return activateSenderGrant(ctx, tx, grant, actor, "global-operator-override", reason)
}

func RevokeSenderGrant(ctx context.Context, tx pgx.Tx, org ID, scope, actor, reason string) error {
	if !validSenderActor(actor, reason, true) {
		return ErrSenderAuthorization
	}
	scope, err := CanonicalSenderScope(scope)
	if err != nil {
		return err
	}
	grant, _, err := ensureSenderGrant(ctx, tx, org, scope)
	if err != nil {
		return err
	}
	if _, err = dbgen.New(tx).
		RevokeSenderGrant(ctx, dbgen.RevokeSenderGrantParams{
			ID:     grant.String(),
			Actor:  actor,
			Reason: reason,
		}); err != nil {
		return err
	}
	if err = invalidateSenderChallenges(ctx, tx, grant); err != nil {
		return err
	}
	return senderGrantEvent(ctx, tx, grant, "revoke", actor, "revocation", reason)
}

func senderGrantFromSQL(row dbgen.GetSenderGrantRow) (SenderGrant, error) {
	result := SenderGrant{
		Scope:      row.Scope,
		Version:    row.Version,
		Active:     row.Active,
		Evidence:   row.Evidence,
		Actor:      row.Actor,
		Reason:     row.Reason,
		VerifiedAt: row.VerifiedAt,
	}
	var err error
	result.ID, err = idFrom(row.ID)
	if err == nil {
		result.Organization, err = idFrom(row.Organization)
	}
	return result, err
}

func getSenderGrant(ctx context.Context, tx pgx.Tx, id ID) (SenderGrant, error) {
	row, err := dbgen.New(tx).GetSenderGrant(ctx, id.String())
	if err != nil {
		return SenderGrant{}, err
	}
	return senderGrantFromSQL(row)
}

func ListSenderGrants(ctx context.Context, tx pgx.Tx, org ID) ([]SenderGrant, error) {
	rows, err := dbgen.New(tx).ListSenderGrants(ctx, org.String())
	if err != nil {
		return nil, err
	}
	result := []SenderGrant{}
	for _, row := range rows {
		grant, err := senderGrantFromSQL(dbgen.GetSenderGrantRow(row))
		if err != nil {
			return nil, err
		}
		result = append(result, grant)
	}
	if len(result) > 1000 {
		return nil, ErrConflict
	}
	return result, nil
}

func (s *Store) AuthorizeSender(ctx context.Context, org ID, from string) (*SenderAuthorization, error) {
	scope, err := CanonicalSenderScope(from)
	if err != nil || !strings.Contains(scope, "@") {
		return nil, ErrSenderAuthorization
	}
	var raw string
	var version int64
	err = func() error {
		sqlcResult, sqlcErr := dbgen.New(s.db.pool).
			StoreAuthorizeSender(ctx, dbgen.StoreAuthorizeSenderParams{
				Organization: org.String(),
				Column2: []string{
					scope,
					senderDomain(scope),
				},
				Scope: scope,
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		raw = sqlcResult.ID
		version = sqlcResult.Version
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSenderAuthorization
	}
	if err != nil {
		return nil, err
	}
	id, err := idFrom(raw)
	if err != nil {
		return nil, err
	}
	return &SenderAuthorization{Grant: id, Version: version}, nil
}

func guardSenderAuthorization(ctx context.Context, tx pgx.Tx, envelope Envelope) error {
	if envelope.Outbound == nil || envelope.Outbound.SenderAuthorization == nil {
		return nil
	}
	proof := envelope.Outbound.SenderAuthorization
	from, err := CanonicalSenderScope(envelope.MailFrom)
	if err != nil || !strings.Contains(from, "@") {
		return ErrSenderAuthorization
	}
	stored, err := dbgen.New(tx).
		GuardSenderAuthorization(ctx, dbgen.GuardSenderAuthorizationParams{
			ID:           proof.Grant.String(),
			Organization: envelope.Organization.String(),
		})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSenderAuthorization
	}
	if err != nil {
		return err
	}
	if !matchesSenderAuthorization(stored, proof.Version, from) {
		return ErrSenderAuthorization
	}
	return nil
}

func validateSenderLocalPart(scope string, at int) error {
	if at < 1 || at > 64 || len(scope) > 254 {
		return ErrSenderAuthorization
	}
	for _, c := range scope[:at] {
		if c <= 32 || c >= 127 || strings.ContainsRune("<>(),;:\"\\[]@", c) {
			return ErrSenderAuthorization
		}
	}
	return nil
}

func validateSenderDomainLabels(domain string) error {
	for label := range strings.SplitSeq(domain, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return ErrSenderAuthorization
		}
		if !validSenderDomainLabel(label) {
			return ErrSenderAuthorization
		}
	}
	return nil
}

func validSenderDomainLabel(label string) bool {
	for _, c := range label {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func matchesSenderChallenge(hash []byte, observedTXT []string) bool {
	matched := false
	for _, value := range observedTXT {
		if len(value) > 4096 {
			continue
		}
		digest := sha256.Sum256([]byte(value))
		if subtle.ConstantTimeCompare(hash, digest[:]) == 1 {
			matched = true
		}
	}
	return matched
}

func matchesSenderAuthorization(stored dbgen.GuardSenderAuthorizationRow, version int64, from string) bool {
	return stored.Active && stored.Version == version && (stored.Scope == from || stored.Scope == senderDomain(from))
}
