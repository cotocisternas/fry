//go:build linux

package hotstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestFileImageExactOriginalAndPartialVerification(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, options())
	body := bytes.Repeat([]byte("image-original-"), 20000)
	loc := appendBytes(t, s, key(t), body)
	size, err := ImageSize(int64(len(body)))
	requireHotNoError(t, err)
	err = s.Close()
	requireHotNoError(t, err)
	image, err := OpenFileImage(dir, loc.File, size)
	requireHotNoError(t, err)
	defer closeForTest(t, image)
	for _, part := range [][2]int64{{0, loc.Size}, {frameSize - 7, 31}, {loc.Size, 0}} {
		var out bytes.Buffer
		{
			err = image.ReadRecord(context.Background(), loc, part[0], part[1], &out)
			requireHotCondition(
				t,
				(err == nil && bytes.Equal(out.Bytes(), body[part[0]:part[0]+part[1]])),
				"file image read changed original",
				part,
				err,
			)
		}
	}
	// Corrupt a requested frame. Both full and partial reads must reject it;
	// a range in a different frame retains the existing range-read semantics.
	corruptFileImageFrame(t, dir, loc, 1)
	var partial bytes.Buffer
	{
		err = image.ReadRecord(context.Background(), loc, frameSize, 1, &partial)
		requireHotCondition(
			t,
			(errors.Is(err, ErrCorrupt) && partial.Len() == 0),
			"unchecked partial frame emitted",
			err,
		)
	}
	var first bytes.Buffer
	{
		err = image.ReadRecord(context.Background(), loc, 0, 1, &first)
		requireHotCondition(
			t,
			(err == nil && first.String() == string(body[:1])),
			"independent frame range changed",
			err,
		)
	}
	{
		err = image.ReadRecord(context.Background(), loc, 0, loc.Size, io.Discard)
		requireHotCondition(t, errors.Is(err, ErrCorrupt), "full image accepted corrupt frame", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	{
		err = image.ReadRecord(ctx, loc, 0, 1, io.Discard)
		requireHotCondition(t, errors.Is(err, context.Canceled), err)
	}
}

func corruptFileImageFrame(t *testing.T, dir string, loc Locator, frame int64) {
	t.Helper()
	// Fixed data.fry under t.TempDir; injects frame corruption; owner: FRY maintainers; expires: 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	f, err := os.OpenFile(filepath.Join(dir, "data.fry"), os.O_RDWR, 0)
	requireHotNoError(t, err)
	at := frameOffset(loc, frame) + 8
	one := []byte{0}
	_, err = f.ReadAt(one, at)
	requireHotNoError(t, err)
	one[0] ^= 1
	_, err = f.WriteAt(one, at)
	requireHotNoError(t, err)
	err = f.Close()
	requireHotNoError(t, err)
}

func TestFileImageOpeningNeverRecoversOrScans(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, options())
	body := []byte("authenticated first original")
	loc := appendBytes(t, s, key(t), body)
	later := appendBytes(t, s, key(t), []byte("unrelated damaged tail"))
	size, err := s.Snapshot()
	requireHotNoError(t, err)
	length := size.Size()
	err = size.Close()
	requireHotNoError(t, err)
	err = s.Close()
	requireHotNoError(t, err)
	sentinel := filepath.Join(dir, "stage-image-sentinel")
	err = os.WriteFile(sentinel, []byte("preserve staging"), 0o600)
	requireHotNoError(t, err)
	// Fixed data.fry under t.TempDir; injects unrelated tail corruption; owner: FRY maintainers; expires:
	// 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	f, err := os.OpenFile(filepath.Join(dir, "data.fry"), os.O_WRONLY, 0)
	requireHotNoError(t, err)
	_, err = f.WriteAt([]byte("bad!"), later.Offset)
	requireHotNoError(t, err)
	closeForTest(t, f)
	// Fixed data.fry under t.TempDir; snapshots bytes before read-only open; owner: FRY maintainers; expires:
	// 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	before, err := os.ReadFile(filepath.Join(dir, "data.fry"))
	requireHotNoError(t, err)
	image, err := OpenFileImage(dir, loc.File, length)
	requireHotCondition(t, err == nil, "read-only open scanned unrelated record", err)
	var out bytes.Buffer
	{
		err = image.ReadRecord(context.Background(), loc, 0, loc.Size, &out)
		requireHotCondition(t, (err == nil && bytes.Equal(out.Bytes(), body)), err)
	}
	err = image.Close()
	requireHotNoError(t, err)
	for _, check := range []struct {
		id     ID
		length int64
	}{{key(t).Receipt, length}, {loc.File, length - 1}, {loc.File, length + 1}} {
		if opened, e := OpenFileImage(dir, check.id, check.length); e == nil {
			closeForTest(t, opened)
			t.Fatal("wrong immutable file identity or length accepted")
		}
	}
	// Fixed data.fry under t.TempDir; verifies read-only open preserved bytes; owner: FRY maintainers; expires:
	// 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	after, readErr := os.ReadFile(filepath.Join(dir, "data.fry"))
	requireHotNoError(t, readErr)
	// Fixed sentinel under t.TempDir; verifies staging was preserved; owner: FRY maintainers; expires: 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	stage, e := os.ReadFile(sentinel)
	requireHotCondition(
		t,
		((e == nil && bytes.Equal(before, after)) && string(stage) == "preserve staging"),
		"read-only image mutated source or staging",
		e,
	)
	checkImageSymlinks(t, dir, loc, length)
}

func checkImageSymlinks(t *testing.T, dir string, loc Locator, length int64) {
	t.Helper()
	var err error
	linked := filepath.Join(t.TempDir(), "linked")
	err = os.Symlink(dir, linked)
	requireHotNoError(t, err)
	if opened, e := OpenFileImage(linked, loc.File, length); e == nil {
		closeForTest(t, opened)
		t.Fatal("symlink directory accepted")
	}
	path := filepath.Join(dir, "data.fry")
	err = os.Rename(path, path+".saved")
	requireHotNoError(t, err)
	err = os.Symlink(path+".saved", path)
	requireHotNoError(t, err)
	if opened, e := OpenFileImage(dir, loc.File, length); e == nil {
		closeForTest(t, opened)
		t.Fatal("symlink data file accepted")
	}
}

func TestFileImageRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.fry")
	requireHotNoError(t, syscall.Mkfifo(path, 0o600))
	id := key(t).Receipt
	done := make(chan error, 1)
	go func() {
		image, err := OpenFileImage(dir, id, 64)
		if image != nil {
			closeForTest(t, image)
		}
		done <- err
	}()
	select {
	case err := <-done:
		requireHotCondition(t, err != nil, "FIFO accepted")
	case <-time.After(time.Second):
		// Join an accidentally blocking O_RDONLY open before failing the test.
		// Fixed FIFO under t.TempDir; nonblocking rescue joins the test reader; owner: FRY maintainers; expires:
		// 2027-01-18.
		// #nosec G304 -- See the path-safety rationale above.
		rescue, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
		requireHotNoError(t, err)
		defer closeForTest(t, rescue)
		select {
		case readErr := <-done:
			if readErr != nil {
				t.Log("rescued read", readErr)
			}
		case <-time.After(time.Second):
			t.Fatal("FIFO reader did not join after rescue writer")
		}
		t.Fatal("nonregular image open waited for a FIFO writer")
	}
}

func TestFileImageRejectsCorruptHeaderWithoutRecovery(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, options())
	loc := appendBytes(t, s, key(t), []byte("original"))
	requireHotNoError(t, s.Close())
	path := filepath.Join(dir, "data.fry")
	// Fixed data.fry under t.TempDir; prepares header corruption; owner: FRY maintainers; expires: 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	data, err := os.ReadFile(path)
	requireHotNoError(t, err)
	data[0] ^= 1
	// Fixed data.fry under t.TempDir; intentional corruption fixture; owner: FRY maintainers; expires: 2027-01-18.
	// #nosec G703 -- See the path-safety rationale above.
	err = os.WriteFile(path, data, 0o600)
	requireHotNoError(t, err)
	stage := filepath.Join(dir, "stage-header-sentinel")
	err = os.WriteFile(stage, []byte("untouched"), 0o600)
	requireHotNoError(t, err)
	if image, e := OpenFileImage(dir, loc.File, int64(len(data))); !errors.Is(e, ErrCorrupt) {
		if image != nil {
			closeForTest(t, image)
		}
		t.Fatal("corrupt immutable header accepted", e)
	}
	// Fixed data.fry under t.TempDir; verifies rejected open preserved bytes; owner: FRY maintainers; expires:
	// 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	after, readErr := os.ReadFile(path)
	requireHotNoError(t, readErr)
	// Fixed sentinel under t.TempDir; verifies rejected open preserved staging; owner: FRY maintainers; expires:
	// 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	sentinel, e := os.ReadFile(stage)
	requireHotCondition(
		t,
		((bytes.Equal(data, after) && e == nil) && string(sentinel) == "untouched"),
		"failed image validation recovered or cleaned source",
		e,
	)
}
