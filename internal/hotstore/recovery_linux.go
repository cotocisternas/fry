//go:build linux

package hotstore

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// IncompleteTailError reports an EOF within a record. It is not permission to
// truncate: the missing bytes may belong to an externally acknowledged locator.
// Open leaves the file unchanged so the coordinator can reconcile first.
type IncompleteTailError struct {
	File                   ID
	ValidEnd, ObservedSize int64
}

func (e *IncompleteTailError) Error() string {
	return fmt.Sprintf(
		"hotstore: incomplete tail in %s after byte %d (file size %d); external reconciliation required",
		e.File,
		e.ValidEnd,
		e.ObservedSize,
	)
}

func scan(f *os.File, options ...Options) (ID, map[Key]Locator, int64, error) {
	var id ID
	records := make(map[Key]Locator)
	info, err := f.Stat()
	if err != nil {
		return id, records, 0, err
	}
	size := info.Size()
	var limits Options
	if len(options) > 0 {
		limits = options[0]
	}
	if limits.MaxFileBytes > 0 && size > limits.MaxFileBytes {
		return id, records, size, ErrCapacity
	}
	head := make([]byte, fileHeaderSize)
	if _, err = f.ReadAt(head, 0); err != nil {
		return id, records, size, fmt.Errorf("%w: file header: %w", ErrCorrupt, err)
	}
	id, err = parseFileHeader(head)
	if err != nil {
		return id, records, size, err
	}
	scanner := newRecordScanner(f, id, size, limits.MaxMessageBytes, limits.MaxRecords > 0)
	err = scanner.scanInto(records, limits.MaxRecords)
	return id, records, size, err
}

// scanInto deliberately retains the verified prefix for explicit tail reconciliation.
func (scanner *recordScanner) scanInto(records map[Key]Locator, maxRecords int64) error {
	size := scanner.size
	for off := int64(fileHeaderSize); off < size; {
		if maxRecords > 0 && int64(len(records)) >= maxRecords {
			return ErrCapacity
		}
		loc, err := scanner.envelope(off)
		if err != nil {
			return err
		}
		if err := scanner.verify(func() error { return nil }, loc); err != nil {
			return err
		}

		if _, exists := records[loc.Key]; exists {
			return fmt.Errorf("%w: duplicate attempt", ErrCorrupt)
		}
		records[loc.Key] = loc
		off += encodedSize(loc.Size)
	}
	return nil
}

// ReconcileTail is an explicit recovery operation, never automatic startup GC.
// protected MUST be the complete authoritative list of still-required locators
// for this file (from the future receipt database, or a test's external ACK log).
// The caller must fence metadata publication before taking that snapshot and
// keep it fenced throughout reconciliation. This local adapter cannot verify
// either prerequisite. A missing/mismatched protected record prevents truncation.
// observed must match the current scan, so stale reports cannot repair new data.
// Interior corruption is never repaired by truncation. No body is reclassified
// as an accepted FRY receipt by this operation.
func ReconcileTail(dir string, observed IncompleteTailError, protected []Locator) (err error) {
	lock, err := lockDirectory(dir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	// Fixed data.fry in caller-owned private directory; exclusive writer lock held during reconciliation; owner:
	// FRY maintainers; expires: 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	f, err := os.OpenFile(filepath.Join(dir, "data.fry"), os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	_, records, _, err := scan(f)
	var current *IncompleteTailError
	if !errors.As(err, &current) {
		if err != nil {
			return err
		}
		return fmt.Errorf("hotstore: no incomplete tail to reconcile")
	}
	if *current != observed {
		return fmt.Errorf("hotstore: stale tail report")
	}
	for _, loc := range protected {
		if got, ok := records[loc.Key]; !ok || got != loc {
			return fmt.Errorf("%w: protected record is absent or damaged", ErrInvalidLocator)
		}
	}
	if err = f.Truncate(current.ValidEnd); err != nil {
		return err
	}
	return f.Sync()
}

// Frame scratch space is allocated once per scan, never per message.
func verifyRecordPayload(checkCanceled func() error, reader io.ReaderAt, loc Locator, buf []byte) error {
	hash := sha256.New()
	for frame := int64(0); frame*frameSize < loc.Size; frame++ {
		if err := checkCanceled(); err != nil {
			return err
		}
		payload, err := readFrame(reader, loc, frame, buf)
		if err != nil {
			return fmt.Errorf("%w at offset %d", err, loc.Offset)
		}
		if _, err := hash.Write(payload); err != nil {
			return err
		}
	}
	if string(hash.Sum(nil)) != string(loc.SHA256[:]) {
		return fmt.Errorf("%w: payload digest at offset %d", ErrCorrupt, loc.Offset)
	}
	return nil
}

func verifyRecordFooter(reader io.ReaderAt, loc Locator, header, footer []byte) error {
	if _, err := reader.ReadAt(footer, loc.Offset+encodedSize(loc.Size)-footerSize); err != nil {
		return err
	}
	if err := checkFooter(footer, header, loc.Size); err != nil {
		return fmt.Errorf("%w: footer at offset %d", err, loc.Offset)
	}
	return nil
}

// recordScanner reuses bounded framing buffers while checking each record.
// The caller decides whether a failed scan may expose its verified prefix.
type recordScanner struct {
	reader                io.ReaderAt
	file                  ID
	size, maxMessage      int64
	bounded               bool
	header, footer, frame []byte
}

func newRecordScanner(reader io.ReaderAt, file ID, size, maxMessage int64, bounded bool) *recordScanner {
	return &recordScanner{
		reader: reader, file: file, size: size, maxMessage: maxMessage, bounded: bounded,
		header: make([]byte, recordHeaderSize), footer: make([]byte, footerSize), frame: make([]byte, frameSize+12),
	}
}

func (scanner *recordScanner) envelope(off int64) (Locator, error) {
	var none Locator
	tail := func() error {
		return &IncompleteTailError{File: scanner.file, ValidEnd: off, ObservedSize: scanner.size}
	}
	if scanner.size-off < recordHeaderSize {
		return none, tail()
	}
	if _, err := scanner.reader.ReadAt(scanner.header, off); err != nil {
		return none, err
	}
	loc, err := parseRecordHeader(scanner.header, scanner.file, off)
	if err != nil {
		return none, fmt.Errorf("%w at offset %d", err, off)
	}
	if scanner.bounded && loc.Size > scanner.maxMessage {
		return none, ErrTooLarge
	}
	if encodedSize(loc.Size) > scanner.size-off {
		return none, tail()
	}
	return loc, nil
}

func (scanner *recordScanner) verify(checkCanceled func() error, loc Locator) error {
	if err := verifyRecordPayload(checkCanceled, scanner.reader, loc, scanner.frame); err != nil {
		return err
	}
	return verifyRecordFooter(scanner.reader, loc, scanner.header, scanner.footer)
}
