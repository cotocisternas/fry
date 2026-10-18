package s3fixture_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/testutil/s3fixture"
)

func snapshotRequest(
	t *testing.T,
	endpoint, method, key string,
	body []byte,
	headers map[string]string,
) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, endpoint+"/fry-test/"+key, bytes.NewReader(body))
	requireFixtureNoError(t, err)
	if method == http.MethodPut {
		digest := sha256.Sum256(body)
		req.Header.Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(digest[:]))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(req)
	requireFixtureNoError(t, err)
	data, err := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("read response: %v / %v", err, closeErr)
	}
	return response.StatusCode, response.Header, data
}

func TestSnapshotRoundTripPreservesHTTPAuthority(t *testing.T) {
	f, err := s3fixture.NewAt(s3fixture.Config{Versioning: "Suspended", Lifecycle: true}, "127.0.0.1:0")
	requireFixtureNoError(t, err)
	t.Cleanup(f.Close)
	endpoint := f.Server.URL
	body := []byte("exact original\x00with binary suffix")
	code, headers, _ := snapshotRequest(t, endpoint, "PUT", "original", body, map[string]string{"If-None-Match": "*"})
	if code != 200 {
		t.Fatalf("put: %d", code)
	}
	originalETag := headers.Get("ETag")
	code, headers, _ = snapshotRequest(
		t,
		endpoint,
		"PUT",
		"retired",
		[]byte{},
		map[string]string{"If-None-Match": "*", "X-Amz-Meta-Fry-Retired": "true"},
	)
	if code != 200 {
		t.Fatalf("retired put: %d", code)
	}
	retiredETag := headers.Get("ETag")
	archive := filepath.Join(t.TempDir(), "state.json")
	if err := f.SaveState(archive); err == nil {
		t.Fatal("export of live HTTP fixture succeeded")
	}
	f.Close()
	requireFixtureNoError(t, f.SaveState(archive))
	originalArchive, err := os.ReadFile(
		archive,
	) // #nosec G304 -- trusted synthetic fixture path; owner: FRY maintainers; review: 2027-01-18
	requireFixtureNoError(t, err)
	if err := f.SaveState(archive); err == nil {
		t.Fatal("snapshot overwrote existing archive")
	}
	unchanged, err := os.ReadFile(
		archive,
	) // #nosec G304 -- trusted synthetic fixture path; owner: FRY maintainers; review: 2027-01-18
	if err != nil || !bytes.Equal(originalArchive, unchanged) {
		t.Fatal("existing archive changed")
	}
	parsed, err := url.Parse(endpoint)
	requireFixtureNoError(t, err)
	restored, err := s3fixture.RestoreAt(archive, parsed.Host)
	requireFixtureNoError(t, err)
	t.Cleanup(restored.Close)
	checkRestoredOriginal(t, endpoint, originalETag, body)
	checkRestoredMarkerAndConfig(t, endpoint, retiredETag)
}

func checkRestoredOriginal(t *testing.T, endpoint, originalETag string, body []byte) {
	t.Helper()
	code, headers, _ := snapshotRequest(t, endpoint, "HEAD", "original", nil, nil)
	if code != 200 || headers.Get("ETag") != originalETag {
		t.Fatalf("HEAD lost identity: %d %v", code, headers)
	}
	digest := sha256.Sum256(body)
	if headers.Get("X-Amz-Checksum-Sha256") != base64.StdEncoding.EncodeToString(digest[:]) {
		t.Fatal("HEAD lost digest")
	}
	code, _, data := snapshotRequest(
		t,
		endpoint,
		"GET",
		"original",
		nil,
		map[string]string{"If-Match": originalETag, "Range": "bytes=6-13"},
	)
	if code != 206 || !bytes.Equal(data, body[6:14]) {
		t.Fatalf("restored range: %d %q", code, data)
	}
	code, _, _ = snapshotRequest(
		t,
		endpoint,
		"PUT",
		"original",
		[]byte("late overwrite"),
		map[string]string{"If-None-Match": "*"},
	)
	if code != 412 {
		t.Fatalf("conditional duplicate: %d", code)
	}
}

func checkRestoredMarkerAndConfig(t *testing.T, endpoint, retiredETag string) {
	t.Helper()
	code, headers, _ := snapshotRequest(t, endpoint, "HEAD", "retired", nil, nil)
	if code != 200 || headers.Get("ETag") != retiredETag || headers.Get("X-Amz-Meta-Fry-Retired") != "true" {
		t.Fatalf("retirement lost: %d %v", code, headers)
	}
	code, _, _ = snapshotRequest(
		t,
		endpoint,
		"PUT",
		"retired",
		[]byte("late publish"),
		map[string]string{"If-None-Match": "*"},
	)
	if code != 412 {
		t.Fatalf("retired key accepted late publication: %d", code)
	}
	code, headers, _ = snapshotRequest(
		t,
		endpoint,
		"PUT",
		"future",
		[]byte("new body"),
		map[string]string{"If-None-Match": "*"},
	)
	if code != 200 || headers.Get("ETag") != "\"opaque-3\"" {
		t.Fatalf("serial not preserved: %d %q", code, headers.Get("ETag"))
	}
	code, _, data := snapshotRequest(t, endpoint, "GET", "?versioning", nil, nil)
	if code != 200 || !bytes.Contains(data, []byte("Suspended")) {
		t.Fatalf("config not restored: %d %q", code, data)
	}
}

func TestSnapshotRejectsInvalidStateBeforeListening(t *testing.T) {
	f, err := s3fixture.NewAt(s3fixture.Config{}, "127.0.0.1:0")
	requireFixtureNoError(t, err)
	endpoint, err := url.Parse(f.Server.URL)
	requireFixtureNoError(t, err)
	snapshotRequest(t, f.Server.URL, "PUT", "key", []byte("body"), map[string]string{"If-None-Match": "*"})
	f.Close()
	archive := filepath.Join(t.TempDir(), "good.json")
	requireFixtureNoError(t, f.SaveState(archive))
	good, err := os.ReadFile(
		archive,
	) // #nosec G304 -- trusted synthetic fixture path; owner: FRY maintainers; review: 2027-01-18
	requireFixtureNoError(t, err)
	tests := map[string]func(map[string]any){
		"version": func(m map[string]any) { m["version"] = 99 },
		"unknown": func(m map[string]any) { m["unexpected"] = true },
		"duplicate key": func(m map[string]any) {
			objects := snapshotObjects(t, m)
			m["objects"] = append(objects, objects[0])
		},
		"digest": func(m map[string]any) {
			snapshotObject(t, m)["digest"] = "0000000000000000000000000000000000000000000000000000000000000000"
		},
		"serial":             func(m map[string]any) { m["serial"] = 0 },
		"missing config":     func(m map[string]any) { delete(m, "config") },
		"missing objects":    func(m map[string]any) { delete(m, "objects") },
		"null body":          func(m map[string]any) { snapshotObject(t, m)["body"] = nil },
		"bad etag":           func(m map[string]any) { snapshotObject(t, m)["etag"] = "caller-controlled" },
		"unsafe key":         func(m map[string]any) { snapshotObject(t, m)["key"] = "../escape" },
		"invalid retirement": func(m map[string]any) { snapshotObject(t, m)["retired"] = true },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			var m map[string]any
			requireFixtureNoError(t, json.Unmarshal(good, &m))
			mutate(m)
			bad, err := json.Marshal(m)
			requireFixtureNoError(t, err)
			path := filepath.Join(t.TempDir(), "bad.json")
			requireFixtureNoError(t, os.WriteFile(path, bad, 0o600))
			restored, err := s3fixture.RestoreAt(path, endpoint.Host)
			if err == nil {
				restored.Close()
				t.Fatal("invalid snapshot accepted")
			}
			probe, err := s3fixture.NewAt(s3fixture.Config{}, endpoint.Host)
			if err != nil {
				t.Fatalf("invalid restore retained listener: %v", err)
			}
			probe.Close()
		})
	}
	checkInvalidSnapshotFiles(t, good, endpoint.Host)
}

func checkInvalidSnapshotFiles(t *testing.T, good []byte, address string) {
	t.Helper()
	for name, data := range map[string][]byte{
		"truncated": good[:len(good)/2],
		"trailing":  append(append([]byte(nil), good...), []byte(" {}")...),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.json")
			requireFixtureNoError(t, os.WriteFile(path, data, 0o600))
			if restored, err := s3fixture.RestoreAt(path, address); err == nil {
				restored.Close()
				t.Fatal("invalid JSON accepted")
			}
		})
	}
	if restored, err := s3fixture.RestoreAt(filepath.Join(t.TempDir(), "missing"), address); err == nil {
		restored.Close()
		t.Fatal("missing archive accepted")
	}
	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "huge.json")
		file, err := os.OpenFile(
			path,
			os.O_CREATE|os.O_EXCL|os.O_WRONLY,
			0o600,
		) // #nosec G304 -- trusted synthetic fixture path; owner: FRY maintainers; review: 2027-01-18
		requireFixtureNoError(t, err)
		requireFixtureNoError(t, file.Truncate((64<<20)+1))
		requireFixtureNoError(t, file.Close())
		if restored, err := s3fixture.RestoreAt(path, address); err == nil {
			restored.Close()
			t.Fatal("oversized archive accepted")
		}
	})
}

func TestSnapshotListenerRejectsNonNumericLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "localhost:0", "192.0.2.1:0", ":0"} {
		t.Run(addr, func(t *testing.T) {
			f, err := s3fixture.NewAt(s3fixture.Config{}, addr)
			if err == nil {
				f.Close()
				t.Fatal("unsafe listener accepted")
			}
		})
	}
}

func snapshotObjects(t *testing.T, m map[string]any) []any {
	t.Helper()
	objects, ok := m["objects"].([]any)
	if !ok || len(objects) == 0 {
		t.Fatal("expected nonempty objects array")
	}
	return objects
}

func snapshotObject(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	object, ok := snapshotObjects(t, m)[0].(map[string]any)
	if !ok {
		t.Fatal("expected object record")
	}
	return object
}
