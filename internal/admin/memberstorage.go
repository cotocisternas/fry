//go:build linux

package admin

import (
	"context"
	"errors"
	"time"

	"github.com/cotocisternas/fry/internal/receiptstore"
)

// authorizeMemberStorage releases its connection before metadata inspection or
// member RPC. A second check prevents a revoked session receiving the snapshot.
func (s *Service) authorizeMemberStorage(ctx context.Context, token string) error {
	tx, err := s.begin(ctx, false)
	if err != nil {
		return err
	}
	who, authErr := authorize(ctx, tx, token)
	if authErr == nil && !allowed(who, "*", false) {
		authErr = ErrForbidden
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return errors.Join(authErr, tx.Rollback(cleanup))
}

// MemberStorage is operator-only deployment diagnostics. Organization grants,
// including content grants covering several organizations, never authorize it.
func (s *Service) MemberStorage(
	ctx context.Context,
	token, member, after string,
	limit int,
) (receiptstore.MemberStorageSnapshot, error) {
	var empty receiptstore.MemberStorageSnapshot
	if err := s.authorizeMemberStorage(ctx, token); err != nil {
		return empty, err
	}
	memberID, err := mailID(member)
	if err != nil || limit < 1 || limit > 64 {
		return empty, ErrInvalid
	}
	var afterID receiptstore.ID
	if after != "" {
		afterID, err = mailID(after)
		if err != nil {
			return empty, ErrInvalid
		}
	}
	snapshot, err := s.mail.MemberStorage(ctx, memberID, afterID, limit)
	if err != nil {
		return empty, err
	}
	if err = s.authorizeMemberStorage(ctx, token); err != nil {
		return empty, err
	}
	return snapshot, nil
}
