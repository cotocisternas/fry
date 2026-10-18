//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

// ErrSubmissionUnauthorized denotes a disabled, rotated, or unavailable proof.
var ErrSubmissionUnauthorized = errors.New("receiptstore: submission credential is no longer authorized")

// NormalizeSubmissionScopes bounds and canonicalizes explicit envelope-sender
// restrictions. There is no wildcard or empty-policy authorization.
func NormalizeSubmissionScopes(senders, domains []string) ([]string, []string, error) {
	if len(senders)+len(domains) < 1 || len(senders)+len(domains) > 128 {
		return nil, nil, ErrSubmissionUnauthorized
	}
	total := 0
	normalize := func(input []string, address bool) ([]string, error) {
		seen := map[string]bool{}
		result := []string{}
		for _, scope := range input {
			total += len(scope)
			if total > 16*1024 {
				return nil, ErrSubmissionUnauthorized
			}
			v, err := CanonicalSenderScope(scope)
			if err != nil || strings.Contains(v, "@") != address {
				return nil, ErrSubmissionUnauthorized
			}
			if !seen[v] {
				result = append(result, v)
				seen[v] = true
			}
		}
		sort.Strings(result)
		return result, nil
	}
	a, err := normalize(senders, true)
	if err != nil {
		return nil, nil, err
	}
	d, err := normalize(domains, false)
	return a, d, err
}

// SubmissionScopeAllows also rejects malformed persisted policies. It is shared
// by the live credential adapter and the durable acceptance guard.
func SubmissionScopeAllows(senders, domains []string, from string) bool {
	a, d, err := NormalizeSubmissionScopes(senders, domains)
	if err != nil {
		return false
	}
	key, err := CanonicalSenderScope(from)
	if err != nil || !strings.Contains(key, "@") {
		return false
	}
	domain := key[strings.LastIndexByte(key, '@')+1:]
	for _, s := range a {
		if s == key {
			return true
		}
	}
	for _, s := range d {
		if s == domain {
			return true
		}
	}
	return false
}

func guardSubmissionAuthorization(ctx context.Context, tx pgx.Tx, e Envelope) error {
	if e.Outbound == nil || e.Outbound.SubmissionAuthorization == nil {
		return nil
	}
	p := e.Outbound.SubmissionAuthorization
	var version int64
	var enabled bool
	var senders, domains []string
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			GuardSubmissionAuthorization(ctx, dbgen.GuardSubmissionAuthorizationParams{
				Username:     p.Username,
				Organization: e.Organization.String(),
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		version = sqlcResult.Version
		enabled = sqlcResult.Enabled
		senders = sqlcResult.AuthorizedSenders
		domains = sqlcResult.AuthorizedDomains
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSubmissionUnauthorized
	}
	if err != nil {
		return err
	}
	if !enabled || version != p.Version || !SubmissionScopeAllows(senders, domains, e.MailFrom) {
		return ErrSubmissionUnauthorized
	}
	return nil
}
