//go:build linux

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/cotocisternas/fry/internal/admin"
	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Startup entries seed missing records only. Persistent administrator edits win
// on subsequent starts, including disabled routes and rotated credentials.
func prepareDirectory(
	ctx context.Context,
	db *pgxpool.Pool,
	store *receiptstore.Store,
	cfg config,
) (*maildirectory.Directory, error) {
	directory := maildirectory.New(db)
	seed := directorySeed{}
	if err := seed.prepareRoutes(ctx, store, cfg); err != nil {
		return nil, err
	}
	if err := seed.prepareIMAP(ctx, store, cfg); err != nil {
		return nil, err
	}
	if err := admin.RegisterConfiguredOrganizations(ctx, db); err != nil {
		return nil, err
	}
	if err := registerSubmissionOrganizations(ctx, db, cfg); err != nil {
		return nil, err
	}
	if err := directory.Seed(ctx, seed.domains, seed.aliases, seed.accounts, seed.passwords); err != nil {
		return nil, err
	}
	return directory, nil
}

type directorySeed struct {
	domains   []maildirectory.Domain
	aliases   []maildirectory.Alias
	accounts  []maildirectory.Account
	passwords []string
}

func (seed *directorySeed) prepareRoutes(ctx context.Context, store *receiptstore.Store, cfg config) error {
	for _, r := range cfg.Routes {
		org, err := parseID(r.Organization)
		if err != nil {
			return err
		}
		key, domain, err := maildirectory.AddressKey(r.Address)
		if err != nil {
			return err
		}
		for _, raw := range r.Inboxes {
			inbox, err := parseID(raw)
			if err != nil {
				return err
			}
			if err = store.EnsureInbox(ctx, org, inbox); err != nil {
				return err
			}
		}
		seed.domains = append(
			seed.domains,
			maildirectory.Domain{Name: domain, Organization: org.String(), Enabled: true},
		)
		seed.aliases = append(
			seed.aliases,
			maildirectory.Alias{
				Address:      key,
				Organization: org.String(),
				Inboxes:      r.Inboxes,
				Enabled:      true,
			},
		)
	}
	return nil
}

func (seed *directorySeed) prepareIMAP(ctx context.Context, store *receiptstore.Store, cfg config) error {
	if cfg.IMAP != nil {
		for _, a := range cfg.IMAP.Accounts {
			org, err := parseID(a.Organization)
			if err != nil {
				return err
			}
			inbox, err := parseID(a.Inbox)
			if err != nil {
				return err
			}
			if err = store.EnsureInbox(ctx, org, inbox); err != nil {
				return err
			}
			password := os.Getenv(a.PasswordEnvironment)
			if password == "" {
				return fmt.Errorf("an IMAP account password environment variable is missing")
			}
			seed.accounts = append(
				seed.accounts,
				maildirectory.Account{
					Username:     a.Username,
					Organization: org.String(),
					Inbox:        inbox.String(),
					Enabled:      true,
				},
			)
			seed.passwords = append(seed.passwords, password)
		}
	}
	return nil
}

func registerSubmissionOrganizations(ctx context.Context, db *pgxpool.Pool, cfg config) error {
	// A configured organization may submit mail without owning an inbox. Make it
	// visible to administrators and assignment workflows without resetting names.
	if cfg.Outbound != nil && cfg.Outbound.Submission != nil {
		for _, account := range cfg.Outbound.Submission.Accounts {
			org, err := parseID(account.Organization)
			if err != nil {
				return err
			}
			if err = admin.RegisterSubmissionOrganization(ctx, db, org); err != nil {
				return err
			}
		}
	}
	return nil
}
