//go:build linux

package maildirectory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cotocisternas/fry/internal/credentials"
	"github.com/cotocisternas/fry/internal/maildirectory/dbgen"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/jackc/pgx/v5"
)

type SubmissionPrincipal struct {
	Username                             string
	Organization                         receiptstore.ID
	Version                              int64
	AuthorizedSenders, AuthorizedDomains []string
}
type SubmissionAccount struct {
	Username          string   `json:"username"`
	Organization      string   `json:"organization"`
	Enabled           bool     `json:"enabled"`
	Version           int64    `json:"version"`
	AuthorizedSenders []string `json:"authorized_senders"`
	AuthorizedDomains []string `json:"authorized_domains"`
}

func validSubmissionUsername(user string) bool {
	return user != "" && len(user) <= 128 && strings.TrimSpace(user) == user && !strings.ContainsAny(user, "\r\n\x00")
}

func (d *Directory) AuthenticateSubmission(ctx context.Context, user, password string) (SubmissionPrincipal, error) {
	var p SubmissionPrincipal
	select {
	case d.passwords <- struct{}{}:
		defer func() { <-d.passwords }()
	case <-ctx.Done():
		return p, ctx.Err()
	}
	var org, hash string
	account, err := dbgen.New(d.db).GetSubmissionCredentials(ctx, user)
	if err == nil {
		p.Username = account.Username
		org = account.Organization
		p.Version = account.Version
		hash = account.PasswordHash
		p.AuthorizedSenders = account.AuthorizedSenders
		p.AuthorizedDomains = account.AuthorizedDomains
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return p, err
	}
	if !credentials.VerifyHash(hash, password) {
		return SubmissionPrincipal{}, ErrNotFound
	}
	p.Organization, err = id(org)
	if err != nil {
		return SubmissionPrincipal{}, err
	}
	p.AuthorizedSenders, p.AuthorizedDomains, err = receiptstore.NormalizeSubmissionScopes(
		p.AuthorizedSenders,
		p.AuthorizedDomains,
	)
	if err != nil {
		return SubmissionPrincipal{}, ErrNotFound
	}
	if err = d.ValidateSubmission(ctx, p); err != nil {
		return SubmissionPrincipal{}, err
	}
	return p, nil
}

func (d *Directory) ValidateSubmission(ctx context.Context, p SubmissionPrincipal) error {
	valid, err := dbgen.New(d.db).
		ValidateSubmissionAccount(
			ctx,
			dbgen.ValidateSubmissionAccountParams{
				Username:     p.Username,
				Organization: p.Organization.String(),
				Version:      p.Version,
			},
		)
	if err != nil {
		return err
	}
	if !valid {
		return ErrNotFound
	}
	return nil
}

// RotateSubmission changes only an existing organization-owned account. The caller
// hashes outside its transaction and commits authorization/audit with this edit.
func RotateSubmission(ctx context.Context, tx pgx.Tx, org receiptstore.ID, user, encoded string) error {
	if !validSubmissionUsername(user) || encoded == "" {
		return ErrInvalid
	}
	tag, err := dbgen.New(tx).
		RotateSubmissionPassword(
			ctx,
			dbgen.RotateSubmissionPasswordParams{
				Username:     user,
				Organization: org.String(),
				PasswordHash: encoded,
			},
		)
	if err != nil {
		return err
	}
	if tag != 1 {
		return ErrNotFound
	}
	return nil
}

func SetSubmissionAccess(ctx context.Context, tx pgx.Tx, org receiptstore.ID, user string, enabled bool) error {
	if !validSubmissionUsername(user) {
		return ErrInvalid
	}
	tag, err := dbgen.New(tx).
		SetSubmissionAccountEnabled(
			ctx,
			dbgen.SetSubmissionAccountEnabledParams{
				Username:     user,
				Organization: org.String(),
				Enabled:      enabled,
			},
		)
	if err != nil {
		return err
	}
	if tag != 1 {
		return ErrNotFound
	}
	return nil
}

// SeedSubmission never overwrites persistent passwords or access state. Existing
// accounts need no bootstrap secret; organization ownership must still match.
func (d *Directory) SeedSubmission(ctx context.Context, accounts []SubmissionAccount, passwords []string) error {
	if len(accounts) != len(passwords) {
		return ErrInvalid
	}
	tx, err := d.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if _, err = dbgen.New(tx).RequireSubmissionSynchronousCommit(ctx); err != nil {
		return err
	}
	// Validate the required fresh schema even for an empty bootstrap list.
	_, err = dbgen.New(tx).CheckSubmissionPolicySchema(ctx)
	if err != nil {
		return fmt.Errorf("maildirectory: submission policy schema required: %w", err)
	}
	order := make([]int, len(accounts))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool { return accounts[order[i]].Username < accounts[order[j]].Username })
	seen := map[string]bool{}
	for _, i := range order {
		a := accounts[i]
		if !validSubmissionUsername(a.Username) || seen[a.Username] {
			return ErrInvalid
		}
		seen[a.Username] = true
		if err := seedSubmissionAccount(ctx, tx, a, passwords[i]); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// seedSubmissionAccount holds the username lock until the caller commits.
// Existing rows retain both their credentials and policy unchanged.
func seedSubmissionAccount(ctx context.Context, tx pgx.Tx, a SubmissionAccount, password string) error {
	if _, err := id(a.Organization); err != nil {
		return err
	}
	// Serialize absent-row insertion across simultaneous process starts.
	if _, err := dbgen.New(tx).LockSeedSubmissionUsername(ctx, a.Username); err != nil {
		return err
	}
	org, err := dbgen.New(tx).LockSubmissionOrganization(ctx, a.Username)
	if err == nil {
		if org != a.Organization {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	senders, domains, e := receiptstore.NormalizeSubmissionScopes(a.AuthorizedSenders, a.AuthorizedDomains)
	if e != nil {
		return ErrInvalid
	}
	encoded, e := credentials.HashPassword(password)
	if e != nil {
		return e
	}
	if _, err := dbgen.New(tx).
		InsertSeedSubmissionAccount(
			ctx,
			dbgen.InsertSeedSubmissionAccountParams{
				Username:          a.Username,
				Organization:      a.Organization,
				PasswordHash:      encoded,
				Enabled:           a.Enabled,
				AuthorizedSenders: senders,
				AuthorizedDomains: domains,
			},
		); err != nil {
		return err
	}
	return nil
}

// CreateSubmission reserves a username permanently. It never changes an existing
// row, including one disabled by an administrator or seeded concurrently.
func CreateSubmission(ctx context.Context, tx pgx.Tx, a SubmissionAccount, encoded string) error {
	if !validSubmissionUsername(a.Username) || encoded == "" {
		return ErrInvalid
	}
	if _, err := id(a.Organization); err != nil {
		return err
	}
	senders, domains, err := receiptstore.NormalizeSubmissionScopes(a.AuthorizedSenders, a.AuthorizedDomains)
	if err != nil {
		return ErrInvalid
	}
	if _, err = dbgen.New(tx).LockNewSubmissionUsername(ctx, a.Username); err != nil {
		return err
	}
	tag, err := dbgen.New(tx).
		InsertSubmissionAccountIfAbsent(
			ctx,
			dbgen.InsertSubmissionAccountIfAbsentParams{
				Username:          a.Username,
				Organization:      a.Organization,
				PasswordHash:      encoded,
				Enabled:           a.Enabled,
				AuthorizedSenders: senders,
				AuthorizedDomains: domains,
			},
		)
	if err != nil {
		return err
	}
	if tag != 1 {
		return ErrConflict
	}
	return nil
}

func SetSubmissionPolicy(
	ctx context.Context,
	tx pgx.Tx,
	org receiptstore.ID,
	user string,
	senders, domains []string,
) error {
	if !validSubmissionUsername(user) {
		return ErrInvalid
	}
	senders, domains, err := receiptstore.NormalizeSubmissionScopes(senders, domains)
	if err != nil {
		return ErrInvalid
	}
	tag, err := dbgen.New(tx).
		UpdateSubmissionPolicy(
			ctx,
			dbgen.UpdateSubmissionPolicyParams{
				Username:          user,
				Organization:      org.String(),
				AuthorizedSenders: senders,
				AuthorizedDomains: domains,
			},
		)
	if err != nil {
		return err
	}
	if tag != 1 {
		return ErrNotFound
	}
	return nil
}
