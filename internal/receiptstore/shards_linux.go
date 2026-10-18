//go:build linux

package receiptstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IngestShard has two dedicated members in distinct failure domains. The complete
// 1..16 shard catalog is immutable after binding; it is not a rebalance API.
type IngestShard struct {
	ID      ID
	Members [2]Member
}
type shardBinding struct {
	ID      string
	Members [2]string
}

func legacyIngestShardID() ID { return ID{15: 1} }

// Version 1 hashes this domain separator and the 16 raw attempt bytes, then
// uses the first eight digest bytes as a big-endian integer modulo catalog size.
func placementIndex(attempt ID, n int) int {
	if n < 1 || n > 16 {
		panic("receiptstore: invalid placement catalog size")
	}
	sum := sha256.Sum256(append([]byte("fry-ingest-attempt-sha256-v1\x00"), attempt[:]...))
	return int((binary.BigEndian.Uint64(sum[:8]) % uint64(n)) & 15)
}

func (s *Store) pairForAttempt(ctx context.Context, attempt ID) ([2]Member, error) {
	var shard string
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(s.db.pool).StorePairForAttempt(ctx, attempt.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		shard = sqlcResult
		return nil
	}()
	if err != nil {
		if !isNoRows(err) {
			return [2]Member{}, err
		}
		return s.shards[placementIndex(attempt, len(s.shards))].Members, nil
	}
	for _, v := range s.shards {
		if v.ID.String() == shard {
			return v.Members, nil
		}
	}
	return [2]Member{}, ErrConflict
}

func bindShards(ctx context.Context, pool *pgxpool.Pool, shards []IngestShard) ([]shardBinding, error) {
	catalog := make([]shardBinding, len(shards))
	for i, v := range shards {
		catalog[i] = shardBinding{
			ID:      v.ID.String(),
			Members: [2]string{memberID(v.Members[0]).String(), memberID(v.Members[1]).String()},
		}
		sort.Strings(catalog[i].Members[:])
	}
	data, err := json.Marshal(catalog)
	if err != nil {
		return nil, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	if _, err = tx.Exec(ctx, `SET LOCAL synchronous_commit = 'on'`); err != nil {
		return nil, err
	}
	if _, err = dbgen.New(tx).BindShardsReadAdvisoryLock(ctx); err != nil {
		return nil, err
	}
	if err = bindShardCatalog(ctx, tx, catalog, data); err != nil {
		return nil, err
	}

	if err = verifyBoundShards(ctx, tx, data); err != nil {
		return nil, err
	}
	if err = commit(ctx, tx); err != nil {
		return nil, err
	}
	return catalog, nil
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

func bindShardCatalog(ctx context.Context, tx pgx.Tx, catalog []shardBinding, data []byte) error {
	stored, err := dbgen.New(tx).BindShardsReadIngestCatalog(ctx)
	if isNoRows(err) {
		return initializeShardCatalog(ctx, tx, catalog, data)
	}
	if err != nil {
		return err
	}
	return validateShardCatalog(ctx, tx, stored, data)
}

func initializeShardCatalog(ctx context.Context, tx pgx.Tx, catalog []shardBinding, data []byte) error {
	history, err := dbgen.New(tx).BindShardsCheckAttempt(ctx)
	if err != nil {
		return err
	}
	if history {
		return fmt.Errorf("receiptstore: unidentified placement history requires offline conversion")
	}
	if _, err = dbgen.New(tx).BindShardsInsertIngestCatalog(ctx, data); err != nil {
		return err
	}
	for _, v := range catalog {
		if _, err = dbgen.New(tx).
			BindShardsInsertIngestShard(ctx, dbgen.BindShardsInsertIngestShardParams{
				ID:      v.ID,
				MemberA: v.Members[0],
				MemberB: v.Members[1],
			}); err != nil {
			return err
		}
	}
	return nil
}

func validateShardCatalog(ctx context.Context, tx pgx.Tx, stored, data []byte) error {
	row, err := dbgen.New(tx).BindShardsReadIngestCatalogVersionAlgorithm(ctx)
	if err != nil {
		return err
	}
	if row.Version != 1 || row.Algorithm != "attempt-sha256-v1" || string(stored) != string(data) {
		// jsonb text formatting is not canonical Go JSON.
		var existing []shardBinding
		if json.Unmarshal(stored, &existing) != nil || row.Version != 1 || row.Algorithm != "attempt-sha256-v1" {
			return ErrConflict
		}
		canonical, err := json.Marshal(existing)
		if err != nil {
			return err
		}
		if string(canonical) != string(data) {
			return ErrConflict
		}
	}
	return nil
}

func verifyBoundShards(ctx context.Context, tx pgx.Tx, data []byte) error {
	rows, err := tx.Query(ctx, dbgen.BindShardsReadIngestShard)
	if err != nil {
		return err
	}
	var actual []shardBinding
	for rows.Next() {
		var v shardBinding
		if err = rows.Scan(&v.ID, &v.Members[0], &v.Members[1]); err != nil {
			break
		}
		actual = append(actual, v)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	actualData, err := json.Marshal(actual)
	if err != nil {
		return err
	}
	if string(actualData) != string(data) {
		return ErrConflict
	}
	return nil
}
