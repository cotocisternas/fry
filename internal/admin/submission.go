//go:build linux

package admin

import (
	"context"

	"github.com/cotocisternas/fry/internal/admin/dbgen"
	"github.com/cotocisternas/fry/internal/credentials"
	"github.com/cotocisternas/fry/internal/maildirectory"

	"github.com/jackc/pgx/v5"
)

// SubmissionAccounts lists live, persisted send-only credentials and explicit
// sender restrictions. Passwords and hashes are never returned.
func (s *Service) SubmissionAccounts(
	ctx context.Context,
	token, org string,
) ([]maildirectory.SubmissionAccount, error) {
	if !validID(org) {
		return nil, ErrInvalid
	}
	tx, err := s.begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	who, err := authorize(ctx, tx, token)
	if err != nil {
		return nil, err
	}
	if !allowed(who, org, false) {
		return nil, ErrForbidden
	}
	rows, err := dbgen.New(tx).ListSubmissionAccounts(ctx, org)
	if err != nil {
		return nil, err
	}
	result := []maildirectory.SubmissionAccount{}
	for _, row := range rows {
		var account maildirectory.SubmissionAccount
		account.Username = row.Username
		account.Organization = row.Organization
		account.Enabled = row.Enabled
		account.Version = row.Version
		account.AuthorizedSenders = row.AuthorizedSenders
		account.AuthorizedDomains = row.AuthorizedDomains
		if account.AuthorizedSenders == nil {
			account.AuthorizedSenders = []string{}
		}
		if account.AuthorizedDomains == nil {
			account.AuthorizedDomains = []string{}
		}
		result = append(result, account)
	}
	return result, nil
}

// RotateSubmissionPassword changes an existing account without enabling it.
// Configuration permission suffices: these credentials grant send-only access.
func (s *Service) RotateSubmissionPassword(ctx context.Context, token, org, username, password string) error {
	if !validID(org) {
		return ErrInvalid
	}
	who, err := s.Identity(ctx, token)
	if err != nil {
		return err
	}
	if !allowed(who, org, false) {
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
	return s.directoryChange(ctx, token, org, "submission-credential-rotate", username, func(tx pgx.Tx) error {
		orgID, err := mailID(org)
		if err != nil {
			return err
		}
		return maildirectory.RotateSubmission(ctx, tx, orgID, username, encoded)
	})
}

func (s *Service) SetSubmissionAccess(ctx context.Context, token, org, username string, enabled bool) error {
	return s.directoryChange(ctx, token, org, "submission-credential-access-update", username, func(tx pgx.Tx) error {
		orgID, err := mailID(org)
		if err != nil {
			return err
		}
		return maildirectory.SetSubmissionAccess(ctx, tx, orgID, username, enabled)
	})
}

// CreateSubmissionAccount is create-only: names remain reserved when disabled.
func (s *Service) CreateSubmissionAccount(
	ctx context.Context,
	token string,
	a maildirectory.SubmissionAccount,
	password string,
) error {
	if !validID(a.Organization) {
		return ErrInvalid
	}
	who, err := s.Identity(ctx, token)
	if err != nil {
		return err
	}
	if !allowed(who, a.Organization, false) {
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
	return s.directoryChange(
		ctx,
		token,
		a.Organization,
		"submission-credential-create",
		a.Username,
		func(tx pgx.Tx) error {
			var exists bool
			exists, err := dbgen.New(tx).OrganizationExistsForSubmission(ctx, a.Organization)
			if err != nil {
				return err
			}
			if !exists {
				return ErrInvalid
			}
			return maildirectory.CreateSubmission(ctx, tx, a, encoded)
		},
	)
}

func (s *Service) SetSubmissionPolicy(
	ctx context.Context,
	token, org, username string,
	senders, domains []string,
) error {
	return s.directoryChange(ctx, token, org, "submission-credential-policy-update", username, func(tx pgx.Tx) error {
		orgID, err := mailID(org)
		if err != nil {
			return err
		}
		return maildirectory.SetSubmissionPolicy(ctx, tx, orgID, username, senders, domains)
	})
}
