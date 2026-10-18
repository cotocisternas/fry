//go:build linux

package receiptstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

// SourceAttempt retrieves the immutable original request for a source identity.
// A retry must use it even if source flags or routing configuration changed.
func (s *Store) SourceAttempt(ctx context.Context, org ID, source Source) (ID, Envelope, Status, error) {
	var id string
	var envelope []byte
	var empty ID
	var e Envelope
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(s.db.pool).
			StoreSourceAttempt(ctx, dbgen.StoreSourceAttemptParams{
				Connection:   source.Connection.String(),
				Folder:       source.Folder,
				Uidvalidity:  int64(source.UIDValidity),
				Uid:          int64(source.UID),
				Organization: org.String(),
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		id = sqlcResult.ID
		envelope = sqlcResult.Envelope
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return empty, e, Status{}, err
	}
	attempt, err := idFrom(id)
	if err != nil {
		return empty, e, Status{}, err
	}
	if err = json.Unmarshal(envelope, &e); err != nil {
		return empty, e, Status{}, err
	}
	status, err := s.Status(ctx, org, attempt)
	return attempt, e, status, err
}

// AcceptedSources queries only a bounded UID batch. It avoids one database
// round-trip per already-collected message without advancing a gap-skipping
// highest-UID checkpoint. The organization filter applies to the source owner.
func (s *Store) AcceptedSources(ctx context.Context, org ID, source Source, uids []uint32) (map[uint32]bool, error) {
	if len(uids) > 1024 {
		return nil, ErrConflict
	}
	numbers := make([]int64, len(uids))
	for i, u := range uids {
		numbers[i] = int64(u)
	}
	rows, err := s.db.pool.Query(
		ctx,
		dbgen.StoreAcceptedSources,
		source.Connection.String(),
		source.Folder,
		int64(source.UIDValidity),
		numbers,
		org.String(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[uint32]bool{}
	for rows.Next() {
		var uid uint32
		if err = rows.Scan(&uid); err != nil {
			return nil, err
		}
		result[uid] = true
	}
	return result, rows.Err()
}

// BindSourceConnection prevents reusing a connection identity for a different
// remote account or organization, which would collide with old source UIDs.
func (s *Store) BindSourceConnection(ctx context.Context, org, connection ID, fingerprint [32]byte) error {
	if org == (ID{}) || connection == (ID{}) {
		return ErrConflict
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if _, err = dbgen.New(tx).
		StoreBindSourceConnectionInsertSourceConnection(
			ctx,
			dbgen.StoreBindSourceConnectionInsertSourceConnectionParams{
				ID:           connection.String(),
				Organization: org.String(),
				Fingerprint:  fingerprint[:],
			},
		); err != nil {
		return err
	}
	var matches bool
	if err = func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			StoreBindSourceConnectionReadSourceConnection(
				ctx,
				dbgen.StoreBindSourceConnectionReadSourceConnectionParams{
					ID:           connection.String(),
					Organization: org.String(),
					Fingerprint:  fingerprint[:],
				},
			)
		if sqlcErr != nil {
			return sqlcErr
		}
		if sqlcResult == nil {
			return fmt.Errorf("receiptstore: unexpected NULL in StoreBindSourceConnectionReadSourceConnection.column_1")
		}
		matches = *sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	if !matches {
		return ErrConflict
	}
	return commit(ctx, tx)
}
