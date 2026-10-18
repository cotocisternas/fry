package testdeps_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	deps "github.com/cotocisternas/fry/tooling/testdeps"

	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
)

func TestRunAcceptsAlreadyRemovedPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	script := "printf '%s' \"$FRY_TEST_POSTGRES_CONTAINER\"; docker rm -f -v " +
		"\"$FRY_TEST_POSTGRES_CONTAINER\" >/dev/null"
	code, err := deps.Run(ctx, []string{"sh", "-c", script}, nil, &stdout, &stderr)
	if code != 0 || err != nil {
		t.Fatalf("already-removed cleanup: code=%d error=%v\n%s", code, err, stderr.String())
	}
	assertRemoved(t, ctx, stdout.String())
}

func TestCloseRemovesPausedPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	fixture, err := deps.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cleanupCancel()
		if err := fixture.Close(cleanupCtx); err != nil {
			t.Error(err)
		}
	})
	docker, _ := checkedDocker(t, ctx)
	if _, err := docker.ContainerPause(ctx, fixture.ContainerID, client.ContainerPauseOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Close(ctx); err != nil {
		t.Fatal(err)
	}
	assertRemoved(t, ctx, fixture.ContainerID)
}

func TestInterruptedStartupRemovesCreatedPostgres(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	docker, images := checkedDocker(t, ctx)
	done := make(chan error, 1)
	go func() {
		fixture, err := deps.Start(ctx)
		if fixture != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cleanupCancel()
			err = errors.Join(err, fixture.Close(cleanupCtx))
		}
		done <- err
	}()
	id := waitForCreatedPostgres(t, ctx, docker, images["postgres"])
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startup cancellation: %v", err)
		}
	case <-time.After(35 * time.Second):
		t.Fatal("startup cleanup exceeded its deadline")
	}
	assertRemoved(t, t.Context(), id)
}

func waitForCreatedPostgres(t *testing.T, ctx context.Context, docker client.APIClient, image string) string {
	t.Helper()
	filters := make(
		client.Filters,
	).Add("ancestor", image).
		Add("label", "org.testcontainers.sessionId="+testcontainers.SessionID())
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		result, err := docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Items) > 0 {
			return result.Items[0].ID
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("no PostgreSQL container created during startup")
		}
	}
}
