package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	deps "github.com/cotocisternas/fry/tooling/testdeps"

	"github.com/testcontainers/testcontainers-go"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code, err := execute(ctx, os.Args[1:])
	cancel()
	if err != nil {
		//nolint:errcheck // A closed stderr cannot be recovered at process exit.
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

func execute(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return 2, fmt.Errorf("usage: test-deps.sh build|check|image postgres|image ryuk|run -- COMMAND [ARGS...]")
	}
	switch args[0] {
	case "image":
		return printImage(args[1:])
	case "build":
		err := build(ctx)
		return errorCode(err), err
	case "check":
		err := check(ctx)
		return errorCode(err), err
	case "run":
		if len(args) < 3 || args[1] != "--" {
			return 2, fmt.Errorf("usage: test-deps.sh run -- COMMAND [ARGS...]")
		}
		// Testcontainers defaults to a parent-PID-derived session, which would
		// merge independent CLI children launched by one rehearsal process.
		if err := os.Setenv("TESTCONTAINERS_SESSION_ID", rand.Text()); err != nil {
			return 1, err
		}
		return deps.Run(ctx, args[2:], os.Stdin, os.Stdout, os.Stderr)
	default:
		return 2, fmt.Errorf("unknown test dependency action %q", args[0])
	}
}

func printImage(args []string) (int, error) {
	if len(args) != 1 {
		return 2, fmt.Errorf("image requires postgres or ryuk")
	}
	tag, _, err := deps.Image(args[0])
	if err != nil {
		return 2, err
	}
	_, err = fmt.Fprintln(os.Stdout, tag)
	return errorCode(err), err
}

func errorCode(err error) int {
	if err != nil {
		return 1
	}
	return 0
}

func build(ctx context.Context) error {
	root := os.Getenv("FRY_TESTDEPS_DIR")
	if root == "" {
		return fmt.Errorf("build requires scripts/test-deps.sh")
	}
	for _, kind := range []string{"postgres", "ryuk"} {
		tag, recipe, err := deps.Image(kind)
		if err != nil {
			return err
		}
		// Owner: FRY tooling; explicit source build uses the checkout path from its wrapper. Revisit
		// 2027-10-18.
		//nolint:gosec // Reviewed call; rationale above.
		command := exec.CommandContext(ctx, "bash", filepath.Join(root, "images/build.sh"), kind, tag, recipe)
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stderr, os.Stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("build %s: %w", kind, err)
		}
	}
	return check(ctx)
}

func check(ctx context.Context) error {
	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		return err
	}
	_, checkErr := deps.Check(ctx, docker)
	return errors.Join(checkErr, docker.Close())
}
