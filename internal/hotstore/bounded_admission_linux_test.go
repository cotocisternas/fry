//go:build linux

package hotstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type boundedCountingReader struct{ reads int }

func (r *boundedCountingReader) Read(p []byte) (int, error) { r.reads++; return 0, io.EOF }

type boundedStageReader struct {
	entered     chan struct{}
	release     chan struct{}
	body        io.Reader
	enteredOnce bool
}

func (r *boundedStageReader) Read(p []byte) (int, error) {
	if !r.enteredOnce {
		r.enteredOnce = true
		close(r.entered)
		<-r.release
	}
	return r.body.Read(p)
}

func boundedHotOptions(records int64) Options {
	size := encodedSize(128)
	return Options{
		MaxMessageBytes:      128,
		MaxBytes:             1 << 20,
		MaxConcurrentAppends: 4,
		MaxRecords:           records,
		MaxFileBytes:         fileHeaderSize + records*size,
	}
}

func TestBoundedHotPendingStageCannotOverbookRecordOrBytes(t *testing.T) {
	for _, capKind := range []string{"records", "bytes"} {
		t.Run(capKind, func(t *testing.T) {
			opts := boundedHotOptions(1)
			if capKind == "bytes" {
				opts.MaxRecords = 8
			}
			s, err := Open(t.TempDir(), opts)
			requireHotNoError(t, err)
			defer closeForTest(t, s)
			first := key(t)
			payload := bytes.Repeat([]byte("a"), 128)
			staged := &boundedStageReader{make(chan struct{}), make(chan struct{}), bytes.NewReader(payload), false}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(staged.release) }) }
			defer release()
			done := make(chan error, 1)
			go func() { _, err := s.Append(context.Background(), first, staged); done <- err }()
			select {
			case <-staged.entered:
			case <-time.After(time.Second):
				t.Fatal("stage not entered")
			}
			rejectedSource := &boundedCountingReader{}
			if _, err = s.Append(context.Background(), key(t), rejectedSource); !errors.Is(err, ErrCapacity) {
				release()
				if stageErr := <-done; stageErr != nil {
					t.Error(stageErr)
				}
				t.Fatal("pending write not reserved", err)
			}
			requireHotCondition(t, rejectedSource.reads == 0, "rejected cap consumed source")
			release()
			err = <-done
			requireHotNoError(t, err)
			loc, err := s.Append(context.Background(), first, bytes.NewReader(payload))
			requireHotCondition(t, err == nil, "identical retry at exact cap", err)
			{
				_, err = s.Append(context.Background(), first, bytes.NewReader(bytes.Repeat([]byte("b"), 128)))
				requireHotCondition(t, errors.Is(err, ErrConflict), "different retry escaped identity", err)
			}
			var out bytes.Buffer
			{
				err = s.Read(context.Background(), loc, 0, 128, &out)
				requireHotCondition(
					t,
					(err == nil && bytes.Equal(out.Bytes(), payload)),
					"bounded original changed",
					err,
				)
			}
			requireHotCondition(t, len(s.Inventory()) == 1, "retry created extra record")
		})
	}
}

func TestBoundedHotExactCapsReopenAndEarlyScanLimit(t *testing.T) {
	for _, capKind := range []string{"records", "bytes"} {
		t.Run(capKind, func(t *testing.T) {
			checkEarlyScanLimit(t, capKind)
		})
	}
}

func TestBoundedHotOptionsAndEncodedSize(t *testing.T) {
	opts := boundedHotOptions(2)
	requireHotNoError(t, ValidateOptions(opts))
	for _, bad := range []Options{
		func() Options { o := opts; o.MaxRecords = 0; return o }(),
		func() Options { o := opts; o.MaxFileBytes = 0; return o }(),
		func() Options { o := opts; o.MaxRecords = -1; return o }(),
		func() Options { o := opts; o.MaxFileBytes = fileHeaderSize; return o }(),
	} {
		requireHotCondition(t, ValidateOptions(bad) != nil, "invalid half/negative/undersized limits accepted")
	}
	root := t.TempDir()
	s, err := Open(root, opts)
	requireHotNoError(t, err)
	defer closeForTest(t, s)
	payload := bytes.Repeat([]byte("a"), 128)
	_, err = s.Append(context.Background(), key(t), bytes.NewReader(payload))
	requireHotNoError(t, err)
	info, err := os.Stat(filepath.Join(root, "data.fry"))
	requireHotNoError(t, err)
	encoded, err := EncodedRecordBytes(int64(len(payload)))
	requireHotCondition(
		t,
		(err == nil && info.Size() == fileHeaderSize+encoded),
		"exported encoded size differs from actual frame bytes",
		err,
	)
}

func TestBoundedHotFailedStageReleasesButLostSyncReplyRetainsOccupancy(t *testing.T) {
	root := t.TempDir()
	opts := boundedHotOptions(1)
	s, err := Open(root, opts)
	requireHotNoError(t, err)
	defer closeForTest(t, s)
	s.hook = func(point string) error {
		if point == "after_stage" {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	{
		_, err = s.Append(context.Background(), key(t), bytes.NewReader([]byte("failed staging")))
		requireHotCondition(t, errors.Is(err, io.ErrUnexpectedEOF), "stage fault missing", err)
	}
	s.hook = func(point string) error {
		if point == "after_sync" {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	originalKey := key(t)
	payload := []byte("durable but reply lost")
	{
		_, err = s.Append(context.Background(), originalKey, bytes.NewReader(payload))
		requireHotCondition(t, errors.Is(err, ErrUncertain), "durable lost reply not uncertain", err)
	}
	err = s.Close()
	requireHotNoError(t, err)
	s, err = Open(root, opts)
	requireHotNoError(t, err)
	defer closeForTest(t, s)
	{
		_, err = s.Append(context.Background(), key(t), bytes.NewReader([]byte("new")))
		requireHotCondition(t, errors.Is(err, ErrCapacity), "durable uncertain record forgot occupancy", err)
	}
	{
		_, err = s.Append(context.Background(), originalKey, bytes.NewReader(payload))
		requireHotCondition(t, err == nil, "durable retry at cap failed", err)
	}
	requireHotCondition(t, len(s.Inventory()) == 1, "retry recharged record")
}

func checkEarlyScanLimit(t *testing.T, capKind string) {
	t.Helper()
	root, exact := exactCapFixture(t)
	var s *Store
	var err error
	path := filepath.Join(root, "data.fry")
	// A cap failure must happen before scanning bytes outside the cap.
	// Fixed data.fry fixture under t.TempDir; opened only to inject corruption; owner: FRY maintainers; expires:
	// 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	file, e := os.OpenFile(path, os.O_WRONLY, 0)
	requireHotNoError(t, e)
	offset := int64(0)
	if capKind == "records" {
		offset = fileHeaderSize + encodedSize(128)
	}
	_, e = file.WriteAt([]byte("bad!"), offset)
	closeErr := file.Close()
	e = errors.Join(e, closeErr)
	requireHotNoError(t, e)

	// Fixed data.fry fixture under t.TempDir; captures bytes before failed scan; owner: FRY maintainers; expires:
	// 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	before, err := os.ReadFile(path)
	requireHotNoError(t, err)
	stage := filepath.Join(root, "stage-limit-sentinel")
	err = os.WriteFile(stage, []byte("preserve"), 0o600)
	requireHotNoError(t, err)
	smaller := exact
	if capKind == "records" {
		smaller.MaxRecords = 1
	} else {
		smaller.MaxFileBytes = boundedHotOptions(1).MaxFileBytes
	}
	s, err = Open(root, smaller)
	if !errors.Is(err, ErrCapacity) {
		if s != nil {
			closeForTest(t, s)
		}
		t.Fatal("over-limit scan did not fail closed", err)
	}
	// Fixed data.fry fixture under t.TempDir; verifies failed scan preserved bytes; owner: FRY maintainers;
	// expires: 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	after, err := os.ReadFile(path)
	requireHotCondition(t, (err == nil && bytes.Equal(before, after)), "limited scan altered data", err)
	{
		// Fixed stage sentinel under t.TempDir verifies preservation; owner: FRY maintainers; expires: 2027-01-18.
		// #nosec G304 -- See the path-safety rationale above.
		raw, err := os.ReadFile(stage)
		requireHotCondition(t, (err == nil && string(raw) == "preserve"), "limited scan cleaned staging", err)
	}
}

func exactCapFixture(t *testing.T) (string, Options) {
	t.Helper()
	root := t.TempDir()
	legacy := options()
	legacy.MaxMessageBytes = 128
	s, err := Open(root, legacy)
	requireHotNoError(t, err)
	for i := range 2 {
		_, err = s.Append(context.Background(), key(t), bytes.NewReader(bytes.Repeat([]byte{byte('a' + i)}, 128)))
		requireHotNoError(t, err)
	}
	err = s.Close()
	requireHotNoError(t, err)
	exact := boundedHotOptions(2)
	s, err = Open(root, exact)
	requireHotCondition(t, err == nil, "exact file limits refuse durable records", err)
	requireHotCondition(t, len(s.Inventory()) == 2, "reopen lost inventory")
	err = s.Close()
	requireHotNoError(t, err)
	return root, exact
}
