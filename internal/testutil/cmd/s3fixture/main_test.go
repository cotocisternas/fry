package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/testutil/s3fixture"
)

// commandOutput makes readiness observable without racing the command's writes.
type commandOutput struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (o *commandOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.buf.Write(p)
	if strings.Contains(o.buf.String(), "FRY S3 fixture listening on ") {
		o.once.Do(func() { close(o.ready) })
	}
	return n, err
}

func (o *commandOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func runUntilReady(t *testing.T, args ...string) (*commandOutput, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	out := &commandOutput{ready: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, out) }()
	t.Cleanup(cancel)
	select {
	case <-out.ready:
		return out, cancel, done
	case err := <-done:
		cancel()
		t.Fatalf("command exited before readiness: %v", err)
	case <-ctx.Done():
		cancel()
		t.Fatal("command did not become ready")
	}
	return nil, nil, nil
}

func awaitCommand(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("command did not stop")
		return nil
	}
}

func TestRunCancellationExportsRestorableState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	out, cancel, done := runUntilReady(t, "-state-out", path)
	cancel()
	if err := awaitCommand(t, done); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "FRY S3 fixture state saved\n") {
		t.Fatalf("missing successful export report: %q", out.String())
	}
	raw, err := os.ReadFile(
		path,
	) // #nosec G304 -- trusted synthetic fixture path; owner: FRY maintainers; review: 2027-01-18
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Version  int    `json:"version"`
		Endpoint string `json:"endpoint"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Version != 1 || !strings.HasPrefix(state.Endpoint, "http://127.0.0.1:") {
		t.Fatalf("invalid snapshot identity: %+v", state)
	}
	// Rebinding the exact endpoint also proves cancellation closed the listener.
	restored, err := s3fixture.RestoreAt(path, strings.TrimPrefix(state.Endpoint, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	restored.Close()
}

func TestRunExportFailureDoesNotReportSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.json")
	const sentinel = "preserve this file"
	if err := os.WriteFile(path, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	out, cancel, done := runUntilReady(t, "-state-out", path)
	cancel()
	if err := awaitCommand(t, done); err == nil {
		t.Fatal("export unexpectedly overwrote existing file")
	}
	if strings.Contains(out.String(), "state saved") {
		t.Fatalf("false export success: %q", out.String())
	}
	raw, err := os.ReadFile(
		path,
	) // #nosec G304 -- trusted synthetic fixture path; owner: FRY maintainers; review: 2027-01-18
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != sentinel {
		t.Fatal("failed export changed existing file")
	}
}

func TestRunMalformedStateNeverReportsReadiness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"endpoint":`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-state-in", path, "-listen", "127.0.0.1:19731"}, &out) }()
	if err := awaitCommand(t, done); err == nil {
		t.Fatal("malformed state unexpectedly started")
	}
	if out.Len() != 0 {
		t.Fatalf("malformed state reported startup: %q", out.String())
	}
}

type failedReadinessOutput struct{ endpoint string }

func (o *failedReadinessOutput) Write(p []byte) (int, error) {
	o.endpoint = strings.TrimSpace(strings.TrimPrefix(string(p), "FRY S3 fixture listening on "))
	return 0, io.ErrClosedPipe
}

func TestRunReadinessOutputFailureClosesListenerWithoutExporting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var out failedReadinessOutput
	err := run(ctx, []string{"-state-out", path}, &out)
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("readiness output error=%v, want closed pipe", err)
	}
	if !strings.HasPrefix(out.endpoint, "http://127.0.0.1:") {
		t.Fatalf("missing attempted readiness endpoint: %q", out.endpoint)
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", strings.TrimPrefix(out.endpoint, "http://"))
	if err != nil {
		t.Fatalf("failed readiness retained the listener: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed startup exported fixture state: %v", err)
	}
}
