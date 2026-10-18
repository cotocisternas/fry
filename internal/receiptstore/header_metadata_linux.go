//go:build linux

package receiptstore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/mail"

	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

const MaxHeaderBytes = 1 << 20

var ErrHeaderUnavailable = errors.New("receiptstore: header unavailable within parsing limit")

// Header returns bounded, immutable original header bytes. Successful metadata
// is derived only from the authenticated receipt stream, never caller input.
// Both persisted hits and population require a live organization-scoped receipt.
func (s *Store) Header(ctx context.Context, org, receipt ID) ([]byte, error) {
	raw, size, found, err := s.loadHeaderMetadata(ctx, org, receipt)
	if err != nil || found {
		return raw, err
	}
	reader, err := s.Open(ctx, org, receipt, 0, min(size, MaxHeaderBytes+1))
	if err != nil {
		return nil, err
	}
	raw, parseErr := readHeaderMetadata(reader)
	closeErr := reader.Close()
	if parseErr != nil || closeErr != nil {
		return nil, errors.Join(parseErr, closeErr)
	}
	if err = s.storeHeaderMetadata(ctx, org, receipt, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func readHeaderMetadata(source io.Reader) ([]byte, error) {
	reader := bufio.NewReaderSize(io.LimitReader(source, MaxHeaderBytes+1), 4096)
	var raw []byte
	lineBytes := 0
	for {
		line, err := reader.ReadSlice('\n')
		if len(raw)+len(line) > MaxHeaderBytes {
			return nil, ErrHeaderUnavailable
		}
		raw = append(raw, line...)
		lineBytes += len(line)
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return nil, fmt.Errorf("%w: %w", ErrHeaderUnavailable, err)
		}
		if err == nil && lineBytes == len(line) && blankHeaderLine(line) {
			break
		}
		if err == nil {
			lineBytes = 0
		}
	}
	if _, err := mail.ReadMessage(bytes.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHeaderUnavailable, err)
	}
	return raw, nil
}

func (s *Store) headerLive(ctx context.Context, tx pgx.Tx, org, receipt ID) (bool, error) {
	state, err := lockReceipt(ctx, tx, org, receipt)
	if err != nil {
		return false, err
	}
	live, err := s.reconcileLifetime(ctx, tx, receipt)
	return state == "live" && live, err
}

// One statement scopes both liveness and metadata to the same snapshot. The
// returned bounded copy belongs only to the current operation; no server cache
// survives it, and subsequent calls cannot reopen a removed receipt.
func (s *Store) loadHeaderMetadata(ctx context.Context, org, receipt ID) ([]byte, int64, bool, error) {
	var raw []byte
	var size int64
	var live bool
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(s.db.pool).
			StoreLoadHeaderMetadata(ctx, dbgen.StoreLoadHeaderMetadataParams{
				ID:           receipt.String(),
				Organization: org.String(),
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		size = sqlcResult.RawSize
		raw = sqlcResult.RawHeader
		if sqlcResult.Column3 == nil {
			return fmt.Errorf("receiptstore: unexpected NULL in StoreLoadHeaderMetadata.Column3")
		}
		live = *sqlcResult.Column3
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, false, ErrNotFound
	}
	if err != nil {
		return nil, 0, false, err
	}
	if !live {
		return nil, 0, false, ErrExpired
	}
	return raw, size, raw != nil, nil
}

func (s *Store) storeHeaderMetadata(ctx context.Context, org, receipt ID, raw []byte) error {
	parsed, err := readHeaderMetadata(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if len(parsed) != len(raw) {
		return ErrHeaderUnavailable
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	live, err := s.headerLive(ctx, tx, org, receipt)
	if err != nil {
		return err
	}
	if !live {
		if err = commit(ctx, tx); err != nil {
			return err
		}
		return ErrExpired
	}
	return insertHeaderMetadata(ctx, tx, receipt, raw)
}

func insertHeaderMetadata(ctx context.Context, tx pgx.Tx, receipt ID, raw []byte) error {
	var existing []byte
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).StoreStoreHeaderMetadataReadHeaderMetadata(ctx, receipt.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		existing = sqlcResult
		return nil
	}()
	if err == nil {
		if !bytes.Equal(existing, raw) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err = dbgen.New(tx).
		StoreStoreHeaderMetadataInsertHeaderMetadata(ctx, dbgen.StoreStoreHeaderMetadataInsertHeaderMetadataParams{
			Receipt:   receipt.String(),
			RawHeader: raw,
		}); err != nil {
		return err
	}
	return commit(ctx, tx)
}

func blankHeaderLine(line []byte) bool {
	return bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n"))
}
