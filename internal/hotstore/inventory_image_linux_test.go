//go:build linux

package hotstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestImageInventoryCompletePhysicalOrderAndPrefix(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, options())
	want := []Locator{
		appendBytes(t, s, key(t), nil),
		appendBytes(t, s, key(t), []byte("second")),
		appendBytes(t, s, key(t), bytes.Repeat([]byte("framed"), 25000)),
	}
	snapshot, err := s.Snapshot()
	requireHotNoError(t, err)
	defer closeForTest(t, snapshot)
	limits := InventoryLimits{MaxRecords: 3, MaxFileBytes: snapshot.Size(), MaxMessageBytes: 200000}
	records, err := snapshot.Inventory(context.Background(), limits)
	requireHotCondition(t, (err == nil && reflect.DeepEqual(records, want)), "physical census differs", records, err)
	exact, err := OpenFileImage(dir, s.FileID(), snapshot.Size())
	requireHotNoError(t, err)
	defer closeForTest(t, exact)
	appendBytes(t, s, key(t), []byte("outside immutable prefix"))
	records, err = snapshot.Inventory(context.Background(), limits)
	requireHotCondition(
		t,
		(err == nil && reflect.DeepEqual(records, want)),
		"grown activefile changed snapshot prefix",
		err,
	)
	{
		partial, e := exact.Inventory(context.Background(), limits)
		requireHotCondition(
			t,
			(errors.Is(e, ErrCorrupt) && partial == nil),
			"exact file image accepted changed length",
			partial,
			e,
		)
	}
}

func TestImageInventoryBoundsBeforeDamagedData(t *testing.T) {
	for _, kind := range []string{"bytes", "records", "message"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			s := openStore(t, dir, options())
			first := appendBytes(t, s, key(t), []byte("first"))
			second := appendBytes(t, s, key(t), bytes.Repeat([]byte("b"), 100))
			snapshot, err := s.Snapshot()
			requireHotNoError(t, err)
			length := snapshot.Size()
			closeForTest(t, snapshot)
			closeForTest(t, s)
			limits := InventoryLimits{MaxRecords: 2, MaxFileBytes: length, MaxMessageBytes: 100}
			path := filepath.Join(dir, "data.fry")
			// Fixed data.fry under t.TempDir; prepares inventory mutation; owner: FRY maintainers; expires:
			// 2027-01-18.
			// #nosec G304 -- See the path-safety rationale above.
			data, err := os.ReadFile(path)
			requireHotNoError(t, err)
			want := ErrCapacity

			switch kind {
			case "bytes":
				limits.MaxFileBytes = length - 1
				data[0] ^= 1
			case "records":
				limits.MaxRecords = 1
				data[second.Offset] ^= 1
			case "message":
				limits.MaxMessageBytes = 99
				data[frameOffset(second, 0)+8] ^= 1
				want = ErrTooLarge
			}
			// Open verifies header before Inventory; byte-limit precedence therefore
			// uses an image opened before this deliberate header mutation.
			image, err := OpenFileImage(dir, first.File, length)
			requireHotNoError(t, err)
			defer closeForTest(t, image)
			// Fixed data.fry under t.TempDir; intentional corruption fixture; owner: FRY maintainers; expires:
			// 2027-01-18.
			// #nosec G703 -- See the path-safety rationale above.
			err = os.WriteFile(path, data, 0o600)
			requireHotNoError(t, err)
			records, err := image.Inventory(context.Background(), limits)
			requireHotCondition(
				t,
				(errors.Is(err, want) && records == nil),
				"inventory read past bound or leaked prefix",
				records,
				err,
			)
		})
	}
}

func TestImageInventoryCorruptionNeverReturnsPrefixOrMutates(t *testing.T) {
	for _, kind := range []string{"header", "frame", "digest", "footer", "duplicate", "tail", "identity"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			s := openStore(t, dir, options())
			first := appendBytes(t, s, key(t), []byte("complete first record"))
			second := appendBytes(t, s, key(t), bytes.Repeat([]byte("second"), 20000))
			snap, err := s.Snapshot()
			requireHotNoError(t, err)
			length := snap.Size()
			closeForTest(t, snap)
			closeForTest(t, s)
			path := filepath.Join(dir, "data.fry")
			// Fixed data.fry under t.TempDir; prepares record mutation; owner: FRY maintainers; expires:
			// 2027-01-18.
			// #nosec G304 -- See the path-safety rationale above.
			data, err := os.ReadFile(path)
			requireHotNoError(t, err)
			data, length = damageInventory(t, data, length, first, second, kind)
			// A post-open identity mutation must still be detected. Other
			// corruptions retain a valid file header and bind their exact size.
			if kind != "identity" {
				// Fixed data.fry under t.TempDir; intentional corruption fixture; owner: FRY maintainers; expires:
				// 2027-01-18.
				// #nosec G703 -- See the path-safety rationale above.
				err = os.WriteFile(path, data, 0o600)
				requireHotNoError(t, err)
			}
			image, err := OpenFileImage(dir, first.File, length)
			requireHotNoError(t, err)
			defer closeForTest(t, image)
			if kind == "identity" {
				// Fixed data.fry under t.TempDir; intentional corruption fixture; owner: FRY maintainers; expires:
				// 2027-01-18.
				// #nosec G703 -- See the path-safety rationale above.
				err = os.WriteFile(path, data, 0o600)
				requireHotNoError(t, err)
			}

			stage := filepath.Join(dir, "stage-inventory-sentinel")
			err = os.WriteFile(stage, []byte("preserve"), 0o600)
			requireHotNoError(t, err)
			records, err := image.Inventory(
				context.Background(),
				InventoryLimits{MaxRecords: 2, MaxFileBytes: length, MaxMessageBytes: 200000},
			)
			requireHotCondition(t, (err != nil && records == nil), "corruption returned usable census", records, err)
			if kind == "tail" {
				var tail *IncompleteTailError
				requireHotCondition(t, errors.As(err, &tail), "incomplete sealed tail was not identified", err)
			}
			after := inventoryReadFile(t, path)
			// Fixed sentinel under t.TempDir; verifies inventory preserved staging; owner: FRY maintainers;
			// expires: 2027-01-18.
			// #nosec G304 -- See the path-safety rationale above.
			sentinel, e := os.ReadFile(stage)
			requireHotCondition(
				t,
				((bytes.Equal(after, data) && e == nil) && string(sentinel) == "preserve"),
				"readonly inventory recovered or cleaned source",
				e,
			)
		})
	}
}

func inventoryReadFile(t *testing.T, path string) []byte {
	t.Helper()
	// Callers pass only their t.TempDir data.fry fixture for byte-preservation checks; owner: FRY maintainers;
	// expires: 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	b, e := os.ReadFile(path)
	requireHotNoError(t, e)
	return b
}

type inventoryCancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *inventoryCancelContext) Err() error {
	c.checks--
	if c.checks == 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestImageInventoryCancellationAndInvalidLimits(t *testing.T) {
	s := openStore(t, t.TempDir(), options())
	appendBytes(t, s, key(t), nil)
	appendBytes(t, s, key(t), bytes.Repeat([]byte("frame"), 40000))
	image, err := s.Snapshot()
	requireHotNoError(t, err)
	defer closeForTest(t, image)
	limits := InventoryLimits{MaxRecords: 2, MaxFileBytes: image.Size(), MaxMessageBytes: 200000}
	for _, invalid := range []InventoryLimits{
		{},
		{
			0,
			image.Size(),
			200000,
		},
		{
			2,
			0,
			200000,
		},
		{
			2,
			image.Size(),
			0,
		},
		{
			-1,
			image.Size(),
			200000,
		},
	} {
		{
			records, e := image.Inventory(context.Background(), invalid)
			requireHotCondition(t, (e != nil && records == nil), "invalid limits accepted", records, e)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	framed := &inventoryCancelContext{Context: ctx, cancel: cancel, checks: 5}
	{
		records, e := image.Inventory(framed, limits)
		requireHotCondition(
			t,
			(errors.Is(e, context.Canceled) && records == nil),
			"frame cancellation returned partial census",
			records,
			e,
		)
	}
	{
		records, e := image.Inventory(context.Background(), limits)
		requireHotCondition(t, (e == nil && len(records) == 2), "cancellation poisoned immutable image", e)
	}
}

func damageInventory(t *testing.T, data []byte, length int64, first, second Locator, kind string) ([]byte, int64) {
	t.Helper()
	switch kind {
	case "header":
		data[second.Offset] ^= 1
	case "frame", "digest":
		at := frameOffset(second, 0)
		data[at+8] ^= 1
		if kind == "digest" {
			putCRC(data[at : at+frameSize+12])
		}
	case "footer":
		data[len(data)-1] ^= 1
	case "duplicate":
		header, err := recordHeader(first.Key, second.Size, second.SHA256)
		requireHotNoError(t, err)
		copy(data[second.Offset:], header)
		end, err := footer(header, second.Size)
		requireHotNoError(t, err)
		copy(data[second.Offset+encodedSize(second.Size)-footerSize:], end)
	case "tail":
		data = data[:len(data)-1]
		length--
	case "identity":
		copy(data[:fileHeaderSize], fileHeader(key(t).Receipt))
	}
	return data, length
}
