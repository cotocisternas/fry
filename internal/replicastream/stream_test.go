package replicastream_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cotocisternas/fry/internal/replicastream"
)

func encoded(t testing.TB, payload []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := replicastream.NewWriter(&b, int64(len(payload)))
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestCompletionAndOriginalBytesWithSmallConsumerReads(t *testing.T) {
	for _, size := range []int{
		0,
		1,
		replicastream.ChunkSize - 1,
		replicastream.ChunkSize,
		replicastream.ChunkSize + 1,
		3 * replicastream.ChunkSize,
	} {
		payload := bytes.Repeat([]byte("x"), size)
		r := replicastream.NewReader(bytes.NewReader(encoded(t, payload)), int64(size))
		var got bytes.Buffer
		buffer := make([]byte, 137)
		if _, err := io.CopyBuffer(&got, readerOnly{r}, buffer); err != nil {
			t.Fatal(size, err)
		}
		if !bytes.Equal(got.Bytes(), payload) {
			t.Fatal("payload changed", size)
		}
		if _, err := r.Read(buffer); !errors.Is(err, io.EOF) {
			t.Fatal("nonsticky completion", err)
		}
	}
}

type readerOnly struct{ io.Reader }

func TestCopyNCannotObserveFinalPayloadBeforeVerifiedCompletion(t *testing.T) {
	payload := bytes.Repeat([]byte("data"), 20000)
	valid := encoded(t, payload)
	cases := map[string][]byte{
		"missing-end": valid[:len(valid)-52],
		"short-end":   valid[:len(valid)-1],
		"extra-byte":  append(append([]byte(nil), valid...), 1),
	}
	corrupt := append([]byte(nil), valid...)
	corrupt[len(corrupt)-10] ^= 1
	cases["bad-checksum"] = corrupt
	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			r := replicastream.NewReader(bytes.NewReader(stream), int64(len(payload)))
			var got bytes.Buffer
			n, err := io.CopyN(&got, r, int64(len(payload)))
			if err == nil || n >= int64(len(payload)) {
				t.Fatal("unverified final bytes escaped", n, err)
			}
			if _, again := r.Read(make([]byte, 1)); !errors.Is(again, err) {
				t.Fatal("terminal error not sticky", again)
			}
		})
	}
	for n := range len(encoded(t, []byte("abc"))) {
		stream := encoded(t, []byte("abc"))[:n]
		r := replicastream.NewReader(bytes.NewReader(stream), 3)
		if got, err := io.ReadAll(r); err == nil || len(got) > 0 {
			t.Fatal("truncation delivered last frame", n, got, err)
		}
	}
}

func TestBudgetsAndWriterFailurePreventCompletion(t *testing.T) {
	var b bytes.Buffer
	w := replicastream.NewWriter(&b, 2)
	if _, err := w.Write([]byte("abc")); !errors.Is(err, replicastream.ErrLimit) {
		t.Fatal(err)
	}
	if err := w.Finish(); !errors.Is(err, replicastream.ErrLimit) || b.Len() != 0 {
		t.Fatal("finished failed stream", err)
	}
	r := replicastream.NewReader(bytes.NewReader(encoded(t, []byte("abc"))), 2)
	if _, err := io.ReadAll(r); !errors.Is(err, replicastream.ErrLimit) {
		t.Fatal(err)
	}
	w = replicastream.NewWriter(shortWriter{}, 10)
	if _, err := w.Write([]byte("abc")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	if err := w.Finish(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestHTTPBodyLengthIsNotStorageCompletion(t *testing.T) {
	// Model a storage reader that returns its bytes, then fails final integrity.
	// HTTP itself successfully delivers all declared response bytes.
	var encodedBody bytes.Buffer
	w := replicastream.NewWriter(&encodedBody, 3)
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		// Deliberately no Finish after source failure.
		if _, err := response.Write(encodedBody.Bytes()); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	r := replicastream.NewReader(response.Body, 3)
	var data strings.Builder
	if n, err := io.CopyN(&data, r, 3); err == nil || n != 0 {
		t.Fatal("HTTP success mistaken for durable completion", n, err)
	}
}

func FuzzFramedStream(f *testing.F) {
	for _, raw := range [][]byte{nil, []byte("FR\x01\x01"), encoded(f, []byte("small")), encoded(f, nil)} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		r := replicastream.NewReader(bytes.NewReader(raw), 1<<20)
		decoded, err := io.ReadAll(r)
		if len(decoded) > 1<<20 {
			t.Fatal("reader exceeded limit")
		}
		if err == nil {
			// Re-encoding a successful stream must preserve the exact decoded bytes.
			again, e := io.ReadAll(replicastream.NewReader(bytes.NewReader(encoded(t, decoded)), 1<<20))
			if e != nil || !bytes.Equal(again, decoded) {
				t.Fatal("unstable stream", e)
			}
		}
	})
}
