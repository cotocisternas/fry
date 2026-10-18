//go:build linux

// Package maildirectory resolves live incoming addresses and mailbox credentials.
// Mutations accept an existing transaction so admin authorization and audits commit
// with the configuration change. No administrator roles live in this module.
package maildirectory

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cotocisternas/fry/db"
	"github.com/cotocisternas/fry/internal/credentials"
	"github.com/cotocisternas/fry/internal/dbschema"
	"github.com/cotocisternas/fry/internal/maildirectory/dbgen"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaDefinition returns only this module's SQL for atomic installation.
func SchemaDefinition() dbschema.Component {
	return dbschema.Component{Name: "maildirectory", SQL: db.MailDirectorySchema()}
}

var (
	ErrNotFound = errors.New("maildirectory: address or account unavailable")
	ErrInvalid  = errors.New("maildirectory: invalid configuration")
	ErrConflict = errors.New("maildirectory: organization ownership conflict")
)

type Directory struct {
	db        *pgxpool.Pool
	passwords chan struct{}
}
type Route struct {
	Organization receiptstore.ID
	Inboxes      []receiptstore.ID
}
type Principal struct {
	Username            string
	Organization, Inbox receiptstore.ID
	Version             int64
}
type Domain struct {
	Name         string `json:"name"`
	Organization string `json:"organization"`
	CatchAll     string `json:"catch_all"`
	Enabled      bool   `json:"enabled"`
}
type Alias struct {
	Address      string   `json:"address"`
	Organization string   `json:"organization"`
	Inboxes      []string `json:"inboxes"`
	Enabled      bool     `json:"enabled"`
}
type Account struct {
	Username     string `json:"username"`
	Organization string `json:"organization"`
	Inbox        string `json:"inbox"`
	Enabled      bool   `json:"enabled"`
}

func New(db *pgxpool.Pool) *Directory { return &Directory{db: db, passwords: make(chan struct{}, 4)} }

func Migrate(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, db.MailDirectorySchema())
	return err
}

func id(raw string) (receiptstore.ID, error) {
	var result receiptstore.ID
	b, err := hex.DecodeString(raw)
	if err != nil || len(b) != 16 || raw != strings.ToLower(raw) {
		return result, ErrInvalid
	}
	copy(result[:], b)
	if result == (receiptstore.ID{}) {
		return result, ErrInvalid
	}
	return result, nil
}

func DomainValid(domain string) bool {
	if domain == "" || len(domain) > 253 {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if !domainLabelValid(label) {
			return false
		}
	}
	return true
}

func domainLabelValid(label string) bool {
	if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, c := range label {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

func AddressKey(address string) (string, string, error) {
	at := strings.LastIndexByte(address, '@')
	if at < 1 || at > 64 || len(address) > 254 {
		return "", "", ErrInvalid
	}
	domain := strings.ToLower(address[at+1:])
	if !DomainValid(domain) {
		return "", "", ErrInvalid
	}
	for _, c := range address[:at] {
		if c <= 32 || c >= 127 || strings.ContainsRune("<>(),;:\"\\[]", c) {
			return "", "", ErrInvalid
		}
	}
	return address[:at] + "@" + domain, domain, nil
}

func (d *Directory) Resolve(ctx context.Context, address string) (Route, error) {
	var result Route
	key, domain, err := AddressKey(address)
	if err != nil {
		return result, ErrNotFound
	}
	// A disabled explicit alias does not fall through to catch-all. One statement
	// sees an internally consistent routing configuration during concurrent edits.
	var org string
	var inboxes []string
	route, err := dbgen.New(d.db).
		ResolveIncomingRoute(ctx, dbgen.ResolveIncomingRouteParams{Address: key, Name: domain})
	if err == nil {
		org = route.Organization
		inboxes = route.Inboxes
	}
	if errors.Is(err, pgx.ErrNoRows) || err == nil && len(inboxes) == 0 {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	result.Organization, err = id(org)
	if err != nil {
		return result, err
	}
	for _, raw := range inboxes {
		inbox, e := id(raw)
		if e != nil {
			return result, e
		}
		result.Inboxes = append(result.Inboxes, inbox)
	}
	return result, nil
}

func (d *Directory) Authenticate(ctx context.Context, username, password string) (Principal, error) {
	var p Principal
	select {
	case d.passwords <- struct{}{}:
		defer func() { <-d.passwords }()
	case <-ctx.Done():
		return p, ctx.Err()
	}
	var hash, org, inbox string
	account, err := dbgen.New(d.db).GetMailAccountCredentials(ctx, username)
	if err == nil {
		p.Username = account.Username
		org = account.Organization
		inbox = account.Inbox
		p.Version = account.Version
		hash = account.PasswordHash
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return p, err
	}
	if !credentials.VerifyHash(hash, password) {
		return p, ErrNotFound
	}
	p.Organization, err = id(org)
	if err != nil {
		return p, err
	}
	p.Inbox, err = id(inbox)
	if err != nil {
		return p, err
	}
	return p, d.Validate(ctx, p)
}

func (d *Directory) Validate(ctx context.Context, p Principal) error {
	valid, err := dbgen.New(d.db).
		ValidateMailAccount(
			ctx,
			dbgen.ValidateMailAccountParams{
				Username:     p.Username,
				Organization: p.Organization.String(),
				Inbox:        p.Inbox.String(),
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

func ownsInbox(ctx context.Context, tx pgx.Tx, org, inbox string) error {
	valid, err := dbgen.New(tx).
		InboxBelongsToOrganization(ctx, dbgen.InboxBelongsToOrganizationParams{ID: inbox, Organization: org})
	if err != nil {
		return err
	}
	if !valid {
		return ErrConflict
	}
	return nil
}

func PutDomain(ctx context.Context, tx pgx.Tx, v Domain, seed bool) error {
	v.Name = strings.ToLower(v.Name)
	if !DomainValid(v.Name) {
		return ErrInvalid
	}
	if _, err := id(v.Organization); err != nil {
		return err
	}
	var catch *string
	if v.CatchAll != "" {
		if err := ownsInbox(ctx, tx, v.Organization, v.CatchAll); err != nil {
			return err
		}
		catch = &v.CatchAll
	}
	_, err := dbgen.New(tx).
		InsertDomainIfAbsent(
			ctx,
			dbgen.InsertDomainIfAbsentParams{
				Name:         v.Name,
				Organization: v.Organization,
				CatchAll:     catch,
				Enabled:      v.Enabled,
			},
		)
	if err != nil {
		return err
	}
	var actual string
	if actual, err = dbgen.New(tx).LockDomainOrganization(ctx, v.Name); err != nil {
		return err
	}
	if actual != v.Organization {
		return ErrConflict
	}
	if seed {
		return nil
	}
	_, err = dbgen.New(tx).
		UpdateDomain(ctx, dbgen.UpdateDomainParams{Name: v.Name, CatchAll: catch, Enabled: v.Enabled})
	return err
}

func PutAlias(ctx context.Context, tx pgx.Tx, v Alias, seed bool) error {
	address, domain, err := AddressKey(v.Address)
	if err != nil || len(v.Inboxes) == 0 || len(v.Inboxes) > 1024 {
		return ErrInvalid
	}
	org, err := aliasOrganization(ctx, tx, domain, v.Organization)
	if err != nil {
		return err
	}
	unique, err := validateAliasTargets(ctx, tx, org, v.Inboxes)
	if err != nil {
		return err
	}
	tag, err := dbgen.New(tx).
		InsertAliasIfAbsent(ctx, dbgen.InsertAliasIfAbsentParams{Address: address, Domain: domain, Enabled: v.Enabled})
	if err != nil {
		return err
	}
	if seed && tag == 0 {
		return nil
	}
	return replaceAliasTargets(ctx, tx, address, v.Enabled, unique)
}

func aliasOrganization(ctx context.Context, tx pgx.Tx, domain, expected string) (string, error) {
	org, err := dbgen.New(tx).LockAliasDomainOrganization(ctx, domain)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalid
	}
	if err != nil {
		return "", err
	}
	if org != expected {
		return "", ErrConflict
	}
	return org, nil
}

func replaceAliasTargets(ctx context.Context, tx pgx.Tx, address string, enabled bool, unique map[string]bool) error {
	if _, err := dbgen.New(tx).
		UpdateAlias(ctx, dbgen.UpdateAliasParams{Address: address, Enabled: enabled}); err != nil {
		return err
	}
	if _, err := dbgen.New(tx).DeleteAliasTargets(ctx, address); err != nil {
		return err
	}
	for inbox := range unique {
		if _, err := dbgen.New(tx).
			CreateAliasTarget(ctx, dbgen.CreateAliasTargetParams{Address: address, Inbox: inbox}); err != nil {
			return err
		}
	}
	return nil
}

func validateAliasTargets(ctx context.Context, tx pgx.Tx, org string, inboxes []string) (map[string]bool, error) {
	unique := map[string]bool{}
	for _, inbox := range inboxes {
		if err := ownsInbox(ctx, tx, org, inbox); err != nil {
			return nil, err
		}
		unique[inbox] = true
	}
	return unique, nil
}

func PutAccount(ctx context.Context, tx pgx.Tx, v Account, encoded string, seed bool) error {
	if !validSubmissionUsername(v.Username) || encoded == "" {
		return ErrInvalid
	}
	if err := ownsInbox(ctx, tx, v.Organization, v.Inbox); err != nil {
		return err
	}
	_, err := dbgen.New(tx).
		InsertMailAccountIfAbsent(
			ctx,
			dbgen.InsertMailAccountIfAbsentParams{
				Username:     v.Username,
				Organization: v.Organization,
				Inbox:        v.Inbox,
				PasswordHash: encoded,
				Enabled:      v.Enabled,
			},
		)
	if err != nil {
		return err
	}
	var org string
	if org, err = dbgen.New(tx).LockMailAccountOrganization(ctx, v.Username); err != nil {
		return err
	}
	if org != v.Organization {
		return ErrConflict
	}
	if seed {
		return nil
	}
	_, err = dbgen.New(tx).
		UpdateMailAccount(
			ctx,
			dbgen.UpdateMailAccountParams{
				Username:     v.Username,
				Inbox:        v.Inbox,
				PasswordHash: encoded,
				Enabled:      v.Enabled,
			},
		)
	return err
}

func (d *Directory) Seed(
	ctx context.Context,
	domains []Domain,
	aliases []Alias,
	accounts []Account,
	passwords []string,
) error {
	if len(accounts) != len(passwords) {
		return ErrInvalid
	}
	hashes, err := seedPasswordHashes(passwords)
	if err != nil {
		return err
	}
	tx, err := d.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if _, err = dbgen.New(tx).RequireDirectorySynchronousCommit(ctx); err != nil {
		return err
	}
	if err := seedDirectoryEntries(ctx, tx, domains, aliases, accounts, hashes); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func seedDirectoryEntries(
	ctx context.Context,
	tx pgx.Tx,
	domains []Domain,
	aliases []Alias,
	accounts []Account,
	hashes []string,
) error {
	for _, v := range domains {
		if err := PutDomain(ctx, tx, v, true); err != nil {
			return fmt.Errorf("seed domain: %w", err)
		}
	}
	for _, v := range aliases {
		if err := PutAlias(ctx, tx, v, true); err != nil {
			return fmt.Errorf("seed alias: %w", err)
		}
	}
	for i, v := range accounts {
		if err := PutAccount(ctx, tx, v, hashes[i], true); err != nil {
			return fmt.Errorf("seed account: %w", err)
		}
	}
	return nil
}

func seedPasswordHashes(passwords []string) ([]string, error) {
	hashes := make([]string, len(passwords))
	for i, password := range passwords {
		encoded, err := credentials.HashPassword(password)
		if err != nil {
			return nil, err
		}
		hashes[i] = encoded
	}
	return hashes, nil
}

func rollback(ctx context.Context, tx pgx.Tx) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := tx.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		slog.ErrorContext(ctx, "mail directory transaction rollback failed")
	}
}
