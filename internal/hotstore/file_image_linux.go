//go:build linux

package hotstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// OpenFileImage opens one immutable, exact-length append file without recovery,
// an index, staging cleanup, or a record scan. The caller owns its lifetime and
// must prevent source replacement/removal until Close has completed.
func OpenFileImage(dir string, expectedFile ID, expectedLength int64) (image *Image, err error) {
	if expectedFile == (ID{}) || expectedLength < fileHeaderSize || expectedLength > 1<<62 {
		return nil, ErrInvalidLocator
	}
	path, err := imageFilePath(dir)
	if err != nil {
		return nil, err
	}
	// Lstat rejects links/nonregular files; O_NOFOLLOW protects final component; caller fences the private
	// directory; owner: FRY maintainers; expires: 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			if closeErr := file.Close(); closeErr != nil {
				err = errors.Join(err, ErrUncertain, closeErr)
			}
		}
	}()
	if err = validateFileImage(file, expectedFile, expectedLength); err != nil {
		return nil, err
	}
	keep = true
	return &Image{
		SectionReader: io.NewSectionReader(file, 0, expectedLength),
		file:          file,
		expectedFile:  expectedFile,
		exactLength:   true,
	}, nil
}

func imageFilePath(dir string) (string, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", ErrCorrupt
	}
	path := filepath.Join(dir, "data.fry")
	info, err = os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", ErrCorrupt
	}
	return path, nil
}

func validateFileImage(file *os.File, expectedFile ID, expectedLength int64) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != expectedLength {
		return ErrCorrupt
	}
	header := make([]byte, fileHeaderSize)
	if _, err = file.ReadAt(header, 0); err != nil {
		return err
	}
	actual, err := parseFileHeader(header)
	if err != nil {
		return err
	}
	if actual != expectedFile {
		return ErrInvalidLocator
	}
	return nil
}

// ReadRecord validates the same record framing, requested frames, and full-read digest
// as ReadImage. It retains no payload after the call and opens no extra files.
func (i *Image) ReadRecord(ctx context.Context, loc Locator, offset, length int64, dst io.Writer) error {
	if i == nil || i.file == nil {
		return ErrClosed
	}
	open := func(ctx context.Context, off, n int64) (io.ReadCloser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if off < 0 || n < 0 || off > i.Size() || n > i.Size()-off {
			return nil, ErrCorrupt
		}
		return io.NopCloser(io.NewSectionReader(i.SectionReader, off, n)), nil
	}
	return ReadImage(ctx, open, loc, offset, length, dst)
}
