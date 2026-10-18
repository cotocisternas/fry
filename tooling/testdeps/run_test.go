package testdeps_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	deps "github.com/cotocisternas/fry/tooling/testdeps"
)

func TestRunPreservesFailureStatusAndCleansUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code, err := deps.Run(
		ctx,
		[]string{
			"sh",
			"-c",
			`printf '%s' "$FRY_TEST_POSTGRES_CONTAINER"; exit 37`,
		},
		nil,
		&stdout,
		&stderr,
	)
	if code != 37 || err == nil {
		t.Fatalf("code = %d, error = %v, want child status 37", code, err)
	}
	assertRemoved(t, ctx, stdout.String())
	if !strings.Contains(stderr.String(), "database system is ready") {
		t.Fatalf("missing PostgreSQL diagnostics: %s", stderr.String())
	}
}

func TestChildFailureKillsDetachedOutputDescendant(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	pidFile := filepath.Join(t.TempDir(), "background-pid")
	script := "sleep 600 </dev/null >/dev/null 2>&1 & printf '%s' \"$!\" > \"$1\"; printf '%s' " +
		"\"$FRY_TEST_POSTGRES_CONTAINER\"; exit 37"
	var stdout, stderr bytes.Buffer
	code, err := deps.Run(ctx, []string{"sh", "-c", script, "sh", pidFile}, nil, &stdout, &stderr)
	pid, parseErr := strconv.Atoi(waitFile(t, pidFile))
	if parseErr != nil || pid < 1 {
		t.Fatalf("invalid descendant pid %d: %v", pid, parseErr)
	}
	t.Cleanup(func() { killProcess(t, pid) })
	if code != 37 || err == nil {
		t.Fatalf("code=%d error=%v, want original child status 37", code, err)
	}
	assertRemoved(t, ctx, stdout.String())
	if processAlive(t, pid) {
		t.Fatalf("descendant %d survived ordinary child failure", pid)
	}
}
