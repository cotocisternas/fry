//go:build linux

package admin

import (
	"context"
	"strings"

	"github.com/cotocisternas/fry/internal/admin/dbgen"
	"github.com/cotocisternas/fry/internal/credentials"
	"github.com/cotocisternas/fry/internal/maildirectory"

	"github.com/jackc/pgx/v5"
)

func (s *Service) directoryChange(
	ctx context.Context,
	token, org, action, target string,
	change func(pgx.Tx) error,
) error {
	if !validID(org) {
		return ErrInvalid
	}
	tx, err := s.begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	who, err := authorize(ctx, tx, token)
	if err != nil {
		return err
	}
	if !allowed(who, org, false) {
		return ErrForbidden
	}
	if err = change(tx); err != nil {
		return err
	}
	if err = audit(ctx, tx, who.ID, action, org, target); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) PutDomain(ctx context.Context, token string, v maildirectory.Domain) error {
	v.Name = strings.ToLower(v.Name)
	return s.directoryChange(
		ctx,
		token,
		v.Organization,
		"incoming-domain-update",
		v.Name,
		func(tx pgx.Tx) error { return maildirectory.PutDomain(ctx, tx, v, false) },
	)
}

func (s *Service) PutAlias(ctx context.Context, token string, v maildirectory.Alias) error {
	return s.directoryChange(
		ctx,
		token,
		v.Organization,
		"alias-update",
		v.Address,
		func(tx pgx.Tx) error { return maildirectory.PutAlias(ctx, tx, v, false) },
	)
}

func (s *Service) PutMailAccount(ctx context.Context, token string, v maildirectory.Account, password string) error {
	who, err := s.Identity(ctx, token)
	if err != nil {
		return err
	}
	if !allowed(who, v.Organization, true) {
		return ErrForbidden
	}
	release, err := s.passwordSlot(ctx)
	if err != nil {
		return err
	}
	defer release()
	encoded, err := credentials.HashPassword(password)
	if err != nil {
		return ErrInvalid
	}
	return s.directoryChange(ctx, token, v.Organization, "mail-credential-update", v.Username, func(tx pgx.Tx) error {
		who, err := authorize(ctx, tx, token)
		if err != nil {
			return err
		}
		if !allowed(who, v.Organization, true) {
			return ErrForbidden
		}
		return maildirectory.PutAccount(ctx, tx, v, encoded, false)
	})
}

func (s *Service) SetMailAccess(ctx context.Context, token, org, username string, enabled bool) error {
	return s.directoryChange(ctx, token, org, "mail-credential-access-update", username, func(tx pgx.Tx) error {
		who, err := authorize(ctx, tx, token)
		if err != nil {
			return err
		}
		if enabled && !allowed(who, org, true) {
			return ErrForbidden
		}
		tag, err := dbgen.New(tx).
			SetMailAccountEnabled(
				ctx,
				dbgen.SetMailAccountEnabledParams{
					Username:     username,
					Organization: org,
					Enabled:      enabled,
				},
			)
		if err == nil && tag != 1 {
			return ErrInvalid
		}
		return err
	})
}

// listDirectory keeps permission checks and the directory query under the same
// shared policy lock. The typed reader borrows this transaction; rollback also
// releases it when authorization, query execution, or row conversion fails.
func listDirectory[T any](
	ctx context.Context,
	s *Service,
	token, org string,
	read func(*dbgen.Queries) ([]T, error),
) ([]T, error) {
	if !validID(org) {
		return nil, ErrInvalid
	}
	tx, err := s.begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	if _, err = authorizeScope(ctx, tx, token, org, false); err != nil {
		return nil, err
	}
	return read(dbgen.New(tx))
}

// Domains projects incoming-domain names and catch-all routing; the shared
// listDirectory helper owns the authorization and transaction lifecycle.
//
//nolint:dupl // Keep this query and domain-specific projection explicit.
func (s *Service) Domains(ctx context.Context, token, org string) ([]maildirectory.Domain, error) {
	return listDirectory(ctx, s, token, org, func(q *dbgen.Queries) ([]maildirectory.Domain, error) {
		rows, err := q.ListDomains(ctx, org)
		if err != nil {
			return nil, err
		}
		result := []maildirectory.Domain{}
		for _, row := range rows {
			result = append(result, maildirectory.Domain{
				Name: row.Name, Organization: row.Organization, CatchAll: row.CatchAll, Enabled: row.Enabled,
			})
		}
		return result, nil
	})
}

// Aliases projects recipient addresses and inbox targets from the alias query.
//
//nolint:dupl // Alias targets have different semantics from domain catch-alls and mailbox credentials.
func (s *Service) Aliases(ctx context.Context, token, org string) ([]maildirectory.Alias, error) {
	return listDirectory(ctx, s, token, org, func(q *dbgen.Queries) ([]maildirectory.Alias, error) {
		rows, err := q.ListAliases(ctx, org)
		if err != nil {
			return nil, err
		}
		result := []maildirectory.Alias{}
		for _, row := range rows {
			result = append(result, maildirectory.Alias{
				Address: row.Address, Organization: row.Organization, Inboxes: row.Inboxes, Enabled: row.Enabled,
			})
		}
		return result, nil
	})
}

// MailAccounts exposes mailbox identities and access state, excluding secrets.
//
//nolint:dupl // Keep the credential-safe projection next to its specific query.
func (s *Service) MailAccounts(ctx context.Context, token, org string) ([]maildirectory.Account, error) {
	return listDirectory(ctx, s, token, org, func(q *dbgen.Queries) ([]maildirectory.Account, error) {
		rows, err := q.ListMailAccounts(ctx, org)
		if err != nil {
			return nil, err
		}
		result := []maildirectory.Account{}
		for _, row := range rows {
			result = append(result, maildirectory.Account{
				Username: row.Username, Organization: row.Organization, Inbox: row.Inbox, Enabled: row.Enabled,
			})
		}
		return result, nil
	})
}
