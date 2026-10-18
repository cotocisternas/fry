//go:build linux

package hotstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"syscall"
	"testing"
)

func TestImageSnapshotBoundsAndIntegrity(t *testing.T) {
	s := openStore(t, t.TempDir(), options())
	body := bytes.Repeat([]byte("message"), 20000)
	loc := appendBytes(t, s, key(t), body)
	image, err := s.Snapshot()
	requireHotNoError(t, err)
	defer closeForTest(t, image)
	want, err := ImageSize(int64(len(body)))
	if err != nil || image.Size() != want {
		t.Fatal("incorrect framing budget", want, image.Size(), err)
	}
	appendBytes(t, s, key(t), []byte("later append must not enter snapshot"))
	data, err := io.ReadAll(image)
	if err != nil || int64(len(data)) != want {
		t.Fatal("snapshot grew with source", len(data), err)
	}
	for _, mutation := range []string{"none", "frame", "rechecksummed-frame", "footer", "truncated"} {
		t.Run(mutation, func(t *testing.T) {
			copyData := bytes.Clone(data)
			copyData = damageImage(copyData, loc, mutation)
			open := func(_ context.Context, off, n int64) (io.ReadCloser, error) {
				return io.NopCloser(io.NewSectionReader(bytes.NewReader(copyData), off, n)), nil
			}
			var out bytes.Buffer
			err := ReadImage(context.Background(), open, loc, 0, loc.Size, &out)
			if mutation == "none" {
				requireHotCondition(
					t,
					(err == nil && bytes.Equal(out.Bytes(), body)),
					"image changed original bytes",
					err,
				)
			} else {
				requireHotCondition(t, errors.Is(err, ErrCorrupt), "corrupt image accepted", err)
			}
			requireHotCondition(t, (mutation != "frame" || out.Len() == 0), "emitted unchecked frame")
		})
	}
}

type imageCloseFailure struct{ io.Reader }

func (imageCloseFailure) Close() error { return syscall.EIO }

func TestReadImageReportsRangeCloseFailures(t *testing.T) {
	s := openStore(t, t.TempDir(), options())
	loc := appendBytes(t, s, key(t), []byte("verified original"))
	image, err := s.Snapshot()
	requireHotNoError(t, err)
	defer closeForTest(t, image)
	for _, failPayload := range []bool{false, true} {
		open := func(_ context.Context, off, n int64) (io.ReadCloser, error) {
			reader := io.NewSectionReader(image, off, n)
			if (off == frameOffset(loc, 0)) == failPayload {
				return imageCloseFailure{reader}, nil
			}
			return io.NopCloser(reader), nil
		}
		{
			err := ReadImage(t.Context(), open, loc, 0, loc.Size, io.Discard)
			requireHotConditionf(t, errors.Is(err, syscall.EIO), "range close failure lost: %v", err)
		}
	}
}

func damageImage(copyData []byte, loc Locator, mutation string) []byte {
	switch mutation {
	case "frame", "rechecksummed-frame":
		start := frameOffset(loc, 0)
		copyData[start+8] ^= 1
		if mutation == "rechecksummed-frame" {
			putCRC(copyData[start : start+frameSize+12])
		}
	case "footer":
		copyData[len(copyData)-1] ^= 1
	case "truncated":
		copyData = copyData[:len(copyData)-1]
	}
	return copyData
}
