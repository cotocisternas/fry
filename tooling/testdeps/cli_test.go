package testdeps_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type runningCLI struct {
	command     *exec.Cmd
	input       io.WriteCloser
	diagnostics bytes.Buffer
	container   string
	databaseURL string
	closed      bool
}

func TestCLISiblingSessionsAreIndependent(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	first := startCLI(t, ctx)
	second := startCLI(t, ctx)
	if first.container == second.container || first.databaseURL == second.databaseURL {
		t.Fatal("sibling invocations shared a PostgreSQL server")
	}
	first.close(t, ctx)
	assertSQL(t, ctx, second.databaseURL)
	second.close(t, ctx)
	third := startCLI(t, ctx)
	third.close(t, ctx)
}

func startCLI(t *testing.T, ctx context.Context, environment ...string) *runningCLI {
	t.Helper()
	descriptor := filepath.Join(t.TempDir(), "descriptor")
	script := "printf '%s\\n%s\\n' \"$FRY_TEST_POSTGRES_CONTAINER\" \"$FRY_TEST_DATABASE_URL\" > " +
		"\"$1\"; cat >/dev/null"
	// Owner: FRY tooling; fixed fixture command writes only this test's temporary descriptor. Revisit
	// 2027-10-18.
	//nolint:gosec // Reviewed call; rationale above.
	command := exec.CommandContext(ctx, "../../scripts/test-deps.sh", "run", "--", "sh", "-c", script, "sh", descriptor)
	command.Env = append(os.Environ(), environment...)
	running := &runningCLI{command: command}
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	running.input = input
	command.Stdout, command.Stderr = &running.diagnostics, &running.diagnostics
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 35*time.Second)
		defer cancel()
		running.close(t, cleanupCtx)
	})
	fields := strings.Fields(waitFile(t, descriptor))
	if len(fields) != 2 {
		t.Fatalf("invalid CLI descriptor: %q", fields)
	}
	running.container, running.databaseURL = fields[0], fields[1]
	return running
}

func (r *runningCLI) close(t *testing.T, ctx context.Context) {
	t.Helper()
	if r.closed {
		return
	}
	r.closed = true
	if err := r.input.Close(); err != nil {
		t.Error(err)
	}
	if err := r.command.Wait(); err != nil {
		t.Errorf("runner exited: %v\n%s", err, r.diagnostics.String())
	}
	if r.container != "" {
		assertRemoved(t, ctx, r.container)
	}
}

func assertSQL(t *testing.T, ctx context.Context, url string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	var value int
	queryErr := conn.QueryRow(ctx, "SELECT 1").Scan(&value)
	closeErr := conn.Close(ctx)
	if queryErr != nil || closeErr != nil || value != 1 {
		t.Fatalf("SQL readiness: value=%d query=%v close=%v", value, queryErr, closeErr)
	}
}
