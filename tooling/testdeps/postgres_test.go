package testdeps_test

import (
	"context"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/jackc/pgx/v5"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"

	deps "github.com/cotocisternas/fry/tooling/testdeps"
)

func TestPostgresReadyAndRemoved(t *testing.T) {
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
	conn, err := pgx.Connect(ctx, fixture.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var version string
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != "180006" {
		t.Fatalf("server version number = %q, want 180006 (18.6)", version)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Close(ctx); err != nil {
		t.Fatal(err)
	}
	assertRemoved(t, ctx, fixture.ContainerID)
}

func assertRemoved(t *testing.T, ctx context.Context, id string) {
	t.Helper()
	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := docker.Close(); err != nil {
			t.Error(err)
		}
	}()
	_, err = docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if !errdefs.IsNotFound(err) {
		t.Fatalf("inspect removed container: %v", err)
	}
}
