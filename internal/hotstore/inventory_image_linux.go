//go:build linux

package hotstore

import (
	"context"
	"fmt"
)

// InventoryLimits caps the records and encoded bytes inspected in one image,
// and the raw size of each original. All limits must be positive.
type InventoryLimits struct {
	MaxRecords, MaxFileBytes, MaxMessageBytes int64
}

// Inventory verifies every complete record in physical order and returns only a
// complete census. It retains no index or payloads on Image. Memory consists of
// one frame buffer and a slice/duplicate-key set bounded by MaxRecords and the
// image length. Errors never return a usable partial census.
//
// The caller owns Close, including on failure, and must fence file mutation and
// removal. OpenFileImage requires an unchanged exact file length; Snapshot keeps
// its immutable-prefix semantics if the active file subsequently grows.
func (i *Image) Inventory(ctx context.Context, limits InventoryLimits) ([]Locator, error) {
	if err := i.validateInventory(ctx, limits); err != nil {
		return nil, err
	}
	size := i.Size()
	id := i.expectedFile
	scanner := newRecordScanner(i.SectionReader, id, size, limits.MaxMessageBytes, true)
	seen := make(map[Key]struct{})
	records := []Locator{}
	for off := int64(fileHeaderSize); off < size; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if int64(len(records)) >= limits.MaxRecords {
			return nil, ErrCapacity
		}
		loc, err := scanner.envelope(off)
		if err != nil {
			return nil, err
		}
		total := encodedSize(loc.Size)

		if _, duplicate := seen[loc.Key]; duplicate {
			return nil, fmt.Errorf("%w: duplicate attempt", ErrCorrupt)
		}
		if err := scanner.verify(ctx.Err, loc); err != nil {
			return nil, err
		}

		seen[loc.Key] = struct{}{}
		records = append(records, loc)
		off += total
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := i.checkInventoryLength(); err != nil {
		return nil, err
	}
	return records, nil
}

func (i *Image) checkInventoryLength() error {
	info, err := i.file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < i.Size() || (i.exactLength && info.Size() != i.Size()) {
		return ErrCorrupt
	}
	return nil
}

func (i *Image) validateInventory(ctx context.Context, limits InventoryLimits) error {
	if !limits.valid() {
		return fmt.Errorf("hotstore: invalid inventory limits")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if i == nil || i.file == nil || i.SectionReader == nil {
		return ErrClosed
	}
	size := i.Size()
	if size > limits.MaxFileBytes {
		return ErrCapacity
	}
	if size < fileHeaderSize {
		return ErrCorrupt
	}
	if err := i.checkInventoryLength(); err != nil {
		return err
	}
	return i.checkInventoryHeader()
}

func (i *Image) checkInventoryHeader() error {
	head := make([]byte, fileHeaderSize)
	if _, err := i.ReadAt(head, 0); err != nil {
		return fmt.Errorf("%w: file header: %w", ErrCorrupt, err)
	}
	id, err := parseFileHeader(head)
	if err != nil {
		return err
	}
	if id != i.expectedFile {
		return ErrInvalidLocator
	}
	return nil
}

func (limits InventoryLimits) valid() bool {
	return limits.MaxRecords > 0 && limits.MaxFileBytes > 0 && limits.MaxMessageBytes > 0
}
