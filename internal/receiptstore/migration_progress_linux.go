//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"time"

	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

type MigrationProgress struct {
	Job, Inbox                                      ID
	Generation                                      int64
	Phase                                           string
	Deadline, CompletedAt                           *time.Time
	CopiedMessages, RemainingFolders, PendingCopies int64
	Manifest                                        []byte
}

func (s *Store) MigrationProgress(ctx context.Context, org, job ID) (MigrationProgress, error) {
	p := MigrationProgress{Job: job}
	var inbox string
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(s.db.pool).
			StoreMigrationProgress(ctx, dbgen.StoreMigrationProgressParams{
				ID:           job.String(),
				Organization: org.String(),
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		inbox = sqlcResult.Inbox
		p.Generation = sqlcResult.Generation
		p.Phase = sqlcResult.Phase
		p.Deadline = sqlcResult.Deadline
		p.CompletedAt = sqlcResult.CompletedAt
		p.Manifest = sqlcResult.Manifest
		p.CopiedMessages = sqlcResult.Count
		p.RemainingFolders = sqlcResult.Count_2
		p.PendingCopies = sqlcResult.Count_3
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	p.Inbox, err = idFrom(inbox)
	return p, err
}
