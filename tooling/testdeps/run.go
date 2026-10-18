package testdeps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Run owns one fixture for the complete child command and all its subprocesses.
// It preserves the child's exit status, inherits stdin for session holders, and
// waits for checked cleanup before returning success.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	fixture, err := Start(ctx)
	if err != nil {
		return 1, err
	}
	code, childErr := runChild(ctx, args, fixture, stdin, stdout, stderr)
	if childErr != nil {
		fixture.Logs(context.WithoutCancel(ctx), stderr)
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	cleanupErr := fixture.Close(cleanupCtx)
	if cleanupErr != nil && code == 0 {
		code = 1
	}
	return code, errors.Join(childErr, cleanupErr)
}

func runChild(
	ctx context.Context,
	args []string,
	fixture *Postgres,
	stdin io.Reader,
	stdout, stderr io.Writer,
) (int, error) {
	if len(args) == 0 {
		return 2, fmt.Errorf("missing child command")
	}
	// The CLI intentionally executes the user's test command without a shell.
	//nolint:gosec // Owner: FRY tooling; reviewed CLI execution boundary, not remote input. Revisit 2027-10-18.
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = append(
		os.Environ(),
		"FRY_TEST_DATABASE_URL="+fixture.DatabaseURL,
		"FRY_TEST_POSTGRES_CONTAINER="+fixture.ContainerID,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return signalGroup(cmd, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	// A completed child may leave descendants that redirected every stream.
	// They still belong to this invocation, including on an ordinary exit.
	killErr := signalGroup(cmd, syscall.SIGKILL)
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	err = errors.Join(err, killErr)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				return 128 + int(status.Signal()), err
			}
			return exitErr.ExitCode(), err
		}
		return 1, err
	}
	return 0, nil
}

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
