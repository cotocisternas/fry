package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	deps "github.com/cotocisternas/fry/tooling/testdeps"

	"github.com/moby/moby/client"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
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
		return 2, fmt.Errorf("usage: test-deps.sh pull|check|image postgres|image ryuk|run -- COMMAND [ARGS...]")
	}
	switch args[0] {
	case "image":
		return printImage(args[1:])
	case "pull":
		err := pull(ctx)
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
	ref, err := deps.Image(args[0])
	if err != nil {
		return 2, err
	}
	_, err = fmt.Fprintln(os.Stdout, ref)
	return errorCode(err), err
}

func errorCode(err error) int {
	if err != nil {
		return 1
	}
	return 0
}

func pull(ctx context.Context) error {
	pullCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	docker, err := testcontainers.NewDockerClientWithOpts(pullCtx)
	if err != nil {
		return err
	}
	err = pullImages(pullCtx, docker)
	return errors.Join(err, docker.Close())
}

func pullImages(ctx context.Context, docker client.APIClient) error {
	for _, kind := range []string{"postgres", "ryuk"} {
		ref, err := deps.Image(kind)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(os.Stderr, "Pulling", ref); err != nil {
			return err
		}
		response, err := docker.ImagePull(ctx, ref, client.ImagePullOptions{
			Platforms: []v1.Platform{{OS: "linux", Architecture: runtime.GOARCH}},
		})
		if err != nil {
			return fmt.Errorf("pull %s: %w", kind, err)
		}
		if err := response.Wait(ctx); err != nil {
			return fmt.Errorf("pull %s: %w", kind, err)
		}
	}
	_, err := deps.Check(ctx, docker)
	return err
}

func check(ctx context.Context) error {
	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		return err
	}
	_, checkErr := deps.Check(ctx, docker)
	return errors.Join(checkErr, docker.Close())
}
