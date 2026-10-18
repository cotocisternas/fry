//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
)

func copyTestStore(t *testing.T, maxMessage int64) *hotstore.Store {
	t.Helper()
	store, err := hotstore.Open(t.TempDir(), hotstore.Options{
		MaxMessageBytes: maxMessage, MaxBytes: 8 << 20, MaxConcurrentAppends: 1,
	})
	requireReceiptNoError(t, err)
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func TestCopyHotRecordPreservesCurrentAndSealedBytes(t *testing.T) {
	for name, body := range map[string][]byte{
		"empty":           nil,
		"multiple frames": bytes.Repeat([]byte("\x00original\r\n"), 20000),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			source := copyTestStore(t, 256<<10)
			original, err := source.Append(
				ctx,
				hotstore.Key{Receipt: newID(t), Attempt: newID(t)},
				bytes.NewReader(body),
			)
			requireReceiptNoError(t, err)
			image, err := source.Snapshot()
			requireReceiptNoError(t, err)
			t.Cleanup(func() { requireReceiptNoError(t, image.Close()) })
			for name, read := range map[string]func(context.Context, hotstore.Locator, int64, int64, io.Writer) error{
				"current": source.Read,
				"sealed":  image.ReadRecord,
			} {
				t.Run(name, func(t *testing.T) {
					next := copyTestStore(t, 256<<10)
					loc, err := copyHotRecord(ctx, next, original, read)
					requireReceiptNoError(t, err)
					if loc.Key != original.Key || loc.File == original.File || loc.Size != int64(len(body)) ||
						loc.SHA256 != sha256.Sum256(body) {
						t.Fatalf("copied locator changed record identity: %+v", loc)
					}
					var got bytes.Buffer
					requireReceiptNoError(t, next.Read(ctx, loc, 0, loc.Size, &got))
					if !bytes.Equal(got.Bytes(), body) {
						t.Fatal("successor changed original bytes")
					}
				})
			}
		})
	}
}

func TestCopyHotRecordRejectsSizeAndDigestChanges(t *testing.T) {
	body := []byte("original bytes")
	original := hotstore.Locator{
		Key:  hotstore.Key{Receipt: newID(t), Attempt: newID(t)},
		Size: int64(len(body)), SHA256: sha256.Sum256(body),
	}
	for name, changed := range map[string][]byte{
		"short":                         body[:len(body)-1],
		"long":                          append(bytes.Clone(body), '!'),
		"same length, different digest": []byte("modified bytes"),
	} {
		t.Run(name, func(t *testing.T) {
			next := copyTestStore(t, 1024)
			read := func(ctx context.Context, loc hotstore.Locator, offset, length int64, dst io.Writer) error {
				if ctx != t.Context() || loc != original || offset != 0 || length != original.Size {
					t.Error("copy did not request the complete original locator with its caller's context")
				}
				_, err := dst.Write(changed)
				return err
			}
			if _, err := copyHotRecord(t.Context(), next, original, read); !errors.Is(err, ErrConflict) {
				t.Fatalf("copy accepted changed content: %v", err)
			}
		})
	}
}

func TestCopyHotRecordSourceFailurePreventsAppend(t *testing.T) {
	next := copyTestStore(t, 1024)
	body := []byte("complete payload followed by a failed source check")
	original := hotstore.Locator{
		Key:  hotstore.Key{Receipt: newID(t), Attempt: newID(t)},
		Size: int64(len(body)), SHA256: sha256.Sum256(body),
	}
	sourceErr := errors.New("source digest verification failed")
	read := func(_ context.Context, _ hotstore.Locator, _, _ int64, dst io.Writer) error {
		_, err := dst.Write(body)
		return errors.Join(err, sourceErr)
	}
	_, err := copyHotRecord(t.Context(), next, original, read)
	if !errors.Is(err, sourceErr) || len(next.Inventory()) != 0 {
		t.Fatalf("source failure appended an unverified record: inventory=%v, err=%v", next.Inventory(), err)
	}
}

func TestCopyHotRecordAppendFailureReleasesAndJoinsSource(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "oversized", true: "closed"}[closed], func(t *testing.T) {
			next := copyTestStore(t, 64)
			want := hotstore.ErrTooLarge
			if closed {
				requireReceiptNoError(t, next.Close())
				want = hotstore.ErrClosed
			}
			body := bytes.Repeat([]byte{'x'}, 256<<10)
			original := hotstore.Locator{
				Key:  hotstore.Key{Receipt: newID(t), Attempt: newID(t)},
				Size: int64(len(body)), SHA256: sha256.Sum256(body),
			}
			sourceErr := errors.New("producer cleanup failed")
			writeResult := make(chan error, 1)
			read := func(_ context.Context, _ hotstore.Locator, _, _ int64, dst io.Writer) error {
				_, err := dst.Write(body)
				writeResult <- err
				return errors.Join(err, sourceErr)
			}
			result := make(chan error, 1)
			go func() { _, err := copyHotRecord(t.Context(), next, original, read); result <- err }()
			select {
			case err := <-result:
				if !errors.Is(err, want) || !errors.Is(err, sourceErr) {
					t.Fatalf("lost append or producer failure: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("append failure stranded the source pipe writer")
			}
			if err := <-writeResult; !errors.Is(err, want) {
				t.Fatalf("producer did not receive append failure: %v", err)
			}
		})
	}
}

func TestCopyHotRecordCancellationPreventsAppend(t *testing.T) {
	next := copyTestStore(t, 1024)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	original := hotstore.Locator{Key: hotstore.Key{Receipt: newID(t), Attempt: newID(t)}, Size: 1024}
	read := func(ctx context.Context, _ hotstore.Locator, _, _ int64, dst io.Writer) error {
		_, err := dst.Write([]byte("prefix"))
		cancel()
		return errors.Join(err, ctx.Err())
	}
	_, err := copyHotRecord(ctx, next, original, read)
	if !errors.Is(err, context.Canceled) || len(next.Inventory()) != 0 {
		t.Fatalf("canceled source appended a partial record: inventory=%v, err=%v", next.Inventory(), err)
	}
}
