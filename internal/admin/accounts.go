//go:build linux

package admin

import (
	"context"

	"github.com/cotocisternas/fry/internal/admin/dbgen"

	"github.com/jackc/pgx/v5"
)

type User struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Method  string  `json:"method"`
	Enabled bool    `json:"enabled"`
	Grants  []Grant `json:"grants"`
}

func (s *Service) Users(ctx context.Context, token string) ([]User, error) {
	tx, err := s.begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	who, err := authorize(ctx, tx, token)
	if err != nil {
		return nil, err
	}
	if !allowed(who, "*", false) {
		return nil, ErrForbidden
	}
	rows, err := dbgen.New(tx).ListAdminUsers(ctx)
	if err != nil {
		return nil, err
	}
	result := []User{}
	for _, row := range rows {
		var u User
		u.ID = row.ID
		u.Name = *row.Name
		u.Method = row.Method
		u.Enabled = row.Enabled
		result = append(result, u)
	}
	for i := range result {
		result[i].Grants, err = grants(ctx, tx, result[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func retainOperator(ctx context.Context, tx pgx.Tx) error {
	exists, err := dbgen.New(tx).HasEnabledOperator(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return ErrConflict
	}
	return nil
}

func (s *Service) SetEnabled(ctx context.Context, token, user string, enabled bool) error {
	if !validID(user) {
		return ErrInvalid
	}
	tx, err := s.begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	who, err := authorize(ctx, tx, token)
	if err != nil {
		return err
	}
	if !allowed(who, "*", false) {
		return ErrForbidden
	}
	tag, err := dbgen.New(tx).SetAdminUserEnabled(ctx, dbgen.SetAdminUserEnabledParams{ID: user, Enabled: enabled})
	if err != nil {
		return err
	}
	if tag != 1 {
		return ErrInvalid
	}
	if err = retainOperator(ctx, tx); err != nil {
		return err
	}
	if err = audit(ctx, tx, who.ID, "account-access-update", "", user); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
