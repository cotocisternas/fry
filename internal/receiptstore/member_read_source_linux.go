//go:build linux

package receiptstore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"
)

var errReadClosureUncertain = errors.New("receiptstore: read descriptor closure unproven")

type memberReadSource struct {
	locator          hotstore.Locator
	directory, state string
	length           int64
}

// The caller holds m.gate for this entire operation, including descriptor close.
// In-process reads already hold that gate through Store.readPinned: acquiring it
// again here would deadlock behind a waiting exclusive maintenance operation.
func (m *LocalMember) acquireRead(ctx context.Context) (func(), error) {
	select {
	case m.readPermits <- struct{}{}:
		return func() { <-m.readPermits }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func resolveMemberRead(ctx context.Context, q queryer, m *LocalMember, pin, org, receipt ID) (memberReadSource, error) {
	var source memberReadSource
	var raw []byte
	var file, attempt string
	var records *int64
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(q).
			ResolveMemberRead(ctx, dbgen.ResolveMemberReadParams{
				ID:           receipt.String(),
				Organization: org.String(),
				Member:       m.id.String(),
				ID_2:         pin.String(),
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		raw = sqlcResult.Locator
		file = sqlcResult.Incarnation
		attempt = sqlcResult.Attempt
		source.directory = sqlcResult.Directory
		source.state = sqlcResult.State
		source.length = sqlcResult.ChargedBytes
		records = sqlcResult.SealedRecords
		return nil
	}()
	if err != nil {
		return source, err
	}
	if !validMemberDirectory(source.directory) {
		return source, ErrConflict
	}
	if err = json.Unmarshal(raw, &source.locator); err != nil {
		return source, err
	}
	if source.locator.File.String() != file || source.locator.Key.Receipt != receipt ||
		source.locator.Key.Attempt.String() != attempt {
		return source, hotstore.ErrInvalidLocator
	}
	if err = validateMemberReadSource(m, source, records); err != nil {
		return source, err
	}
	return source, nil
}

func (m *LocalMember) readSource(
	ctx context.Context,
	source memberReadSource,
	offset, length int64,
	dst io.Writer,
) error {
	if source.state == "current" {
		return m.bytes.Read(ctx, source.locator, offset, length, dst)
	}
	image, err := hotstore.OpenFileImage(filepath.Join(m.root, source.directory), source.locator.File, source.length)
	if err != nil {
		if errors.Is(err, hotstore.ErrUncertain) {
			m.readClosureUncertain.Store(true)
			return errors.Join(errReadClosureUncertain, err)
		}
		return err
	}
	readErr := image.ReadRecord(ctx, source.locator, offset, length, dst)
	closeImage := image.Close
	if m.imageClose != nil {
		closeImage = func() error { return m.imageClose(image) }
	}
	if closeErr := closeImage(); closeErr != nil {
		m.readClosureUncertain.Store(true)
		return errors.Join(errReadClosureUncertain, readErr, closeErr)
	}
	return readErr
}

func validateMemberReadSource(m *LocalMember, source memberReadSource, records *int64) error {
	if source.state == "current" {
		if source.locator.File != m.bytes.FileID() || source.directory != m.directory {
			return ErrStale
		}
	} else if records == nil || *records < 1 || source.length < 64 || source.locator.File == m.bytes.FileID() {
		return ErrConflict
	}
	return nil
}
