//go:build linux

package hotstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
)

// Image is an immutable-length view of a synced append-file prefix. The caller
// must close it before removing the source directory. No GC runs in hotstore.
type Image struct {
	*io.SectionReader
	file         *os.File
	expectedFile ID
	exactLength  bool
}

func (i *Image) Close() error { return i.file.Close() }
func (s *Store) Snapshot() (*Image, error) {
	s.life.RLock()
	defer s.life.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.poisoned {
		return nil, ErrUncertain
	}
	file, err := os.Open(s.file.Name())
	if err != nil {
		return nil, err
	}
	return &Image{SectionReader: io.NewSectionReader(file, 0, s.size), file: file, expectedFile: s.id}, nil
}

// ImageSize accounts for framing, including empty records and the file header.
func ImageSize(rawSizes ...int64) (int64, error) {
	total := int64(fileHeaderSize)
	for _, n := range rawSizes {
		if n < 0 || n > maxMessageSize {
			return 0, ErrTooLarge
		}
		if total > 1<<62-encodedSize(n) {
			return 0, ErrTooLarge
		}
		total += encodedSize(n)
	}
	return total, nil
}

// RangeOpener opens an exact immutable byte range; its implementation must bind
// reads to the published object generation (for example with an ETag condition).
type RangeOpener func(context.Context, int64, int64) (io.ReadCloser, error)

// ReadImage verifies the same format as local Read. It uses three small range
// reads (file header, record header, footer) and one contiguous payload-frame
// range, regardless of payload size. Empty reads need no payload request.
func ReadImage(ctx context.Context, open RangeOpener, loc Locator, offset, length int64, dst io.Writer) (err error) {
	if open == nil || dst == nil || !validImageRange(loc, offset, length) {
		return ErrInvalidLocator
	}
	header := make([]byte, fileHeaderSize)
	if err := exactRange(ctx, open, 0, header); err != nil {
		return err
	}
	id, err := parseFileHeader(header)
	if err != nil {
		return err
	}
	if id != loc.File {
		return ErrInvalidLocator
	}
	source := &imageRanges{ctx: ctx, open: open}
	if length > 0 {
		first, last := offset/frameSize, (offset+length-1)/frameSize
		source.start = frameOffset(loc, first)
		source.end = frameOffset(loc, last) + min(int64(frameSize), loc.Size-last*frameSize) + 12
		source.next = source.start
	}
	defer func() { err = errors.Join(err, source.close()) }()
	return readRecord(ctx, source, loc, offset, length, dst)
}

func exactRange(ctx context.Context, open RangeOpener, offset int64, b []byte) (err error) {
	r, err := open(ctx, offset, int64(len(b)))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	_, err = io.ReadFull(r, b)
	return err
}

type imageRanges struct {
	ctx              context.Context
	open             RangeOpener
	body             io.ReadCloser
	start, end, next int64
}

func (r *imageRanges) close() error {
	if r.body != nil {
		return r.body.Close()
	}
	return nil
}

func (r *imageRanges) ReadAt(b []byte, off int64) (int, error) {
	if off >= r.start && off < r.end && r.end > r.start {
		if off != r.next || int64(len(b)) > r.end-off {
			return 0, ErrCorrupt
		}
		if r.body == nil {
			var err error
			r.body, err = r.open(r.ctx, r.start, r.end-r.start)
			if err != nil {
				return 0, err
			}
		}
		n, err := io.ReadFull(r.body, b)
		r.next += int64(n)
		return n, err
	}
	if err := exactRange(r.ctx, r.open, off, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func readRecord(ctx context.Context, source io.ReaderAt, loc Locator, offset, length int64, dst io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyRecordEnvelope(source, loc); err != nil {
		return err
	}
	full := offset == 0 && length == loc.Size
	hash := sha256.New()
	if err := streamRecordRange(ctx, source, loc, offset, length, dst, hash, full); err != nil {
		return err
	}
	if full && string(hash.Sum(nil)) != string(loc.SHA256[:]) {
		return ErrCorrupt
	}
	return ctx.Err()
}

func streamRecordRange(
	ctx context.Context,
	source io.ReaderAt,
	loc Locator,
	offset, length int64,
	dst, hash io.Writer,
	full bool,
) error {
	buf := make([]byte, frameSize+12)
	stop := offset + length
	for index := offset / frameSize; length > 0 && index*frameSize < stop; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		payload, e := readFrame(source, loc, index, buf)
		if e != nil {
			return e
		}
		if err := hashPayload(hash, payload, full); err != nil {
			return err
		}

		start := max(offset-index*frameSize, int64(0))
		end := min(stop-index*frameSize, int64(len(payload)))
		if err := writeAll(dst, payload[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func verifyRecordEnvelope(source io.ReaderAt, loc Locator) error {
	header := make([]byte, recordHeaderSize)
	if _, err := source.ReadAt(header, loc.Offset); err != nil {
		return fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	actual, err := parseRecordHeader(header, loc.File, loc.Offset)
	if err != nil {
		return err
	}
	if actual != loc {
		return ErrCorrupt
	}
	end := make([]byte, footerSize)
	if _, err = source.ReadAt(end, loc.Offset+encodedSize(loc.Size)-footerSize); err != nil {
		return fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	if err = checkFooter(end, header, loc.Size); err != nil {
		return err
	}
	return nil
}

func validImageRange(loc Locator, offset, length int64) bool {
	return loc.Offset >= fileHeaderSize && loc.Size >= 0 && loc.Size <= maxMessageSize && loc.Offset <= 1<<62 &&
		offset >= 0 &&
		length >= 0 &&
		offset <= loc.Size &&
		length <= loc.Size-offset
}

func hashPayload(hash io.Writer, payload []byte, full bool) error {
	if !full {
		return nil
	}
	_, err := hash.Write(payload)
	return err
}
