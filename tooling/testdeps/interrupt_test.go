package testdeps_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	deps "github.com/cotocisternas/fry/tooling/testdeps"
)

func TestInterruptRemovesFixtureAndKillsResistantDescendant(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pidFile := filepath.Join(t.TempDir(), "descendant")
	containerFile := filepath.Join(t.TempDir(), "container")
	command := "sh -c 'trap \"\" TERM; echo $$ > \"$1\"; exec sleep 600' sh \"$1\" & printf %s " +
		"\"$FRY_TEST_POSTGRES_CONTAINER\" > \"$2\"; wait"
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := deps.Run(
			ctx,
			[]string{"sh", "-c", command, "sh", pidFile, containerFile},
			nil,
			io.Discard,
			io.Discard,
		)
		done <- result{code, err}
	}()
	pid, err := strconv.Atoi(strings.TrimSpace(waitFile(t, pidFile)))
	if err != nil || pid < 1 {
		t.Fatalf("invalid descendant pid: %d %v", pid, err)
	}
	t.Cleanup(func() { killProcess(t, pid) })
	id := waitFile(t, containerFile)
	cancel()
	select {
	case value := <-done:
		if value.code != 143 || value.err == nil {
			t.Fatalf("exit status = %d, error = %v; want 143 and an interruption error", value.code, value.err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("runner did not finish bounded cleanup")
	}
	if processAlive(t, pid) {
		t.Fatalf("descendant %d survived runner cancellation", pid)
	}
	assertRemoved(t, t.Context(), id)
}

func waitFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		//nolint:gosec // Owner: FRY tooling; path is this test's temporary descriptor. Revisit 2027-10-18.
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return string(data)
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s: %v", path, err)
		case <-ticker.C:
		}
	}
}

func processAlive(t *testing.T, pid int) bool {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	_, state, ok := strings.Cut(string(data), ") ")
	if !ok {
		t.Fatalf("invalid process stat: %s", data)
	}
	return !strings.HasPrefix(state, "Z ")
}

func killProcess(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Error(err)
	}
}
