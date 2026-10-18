// Package testdeps owns isolated, source-built dependencies for native FRY tests.
package testdeps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/containerd/errdefs"
	"github.com/jackc/pgx/v5"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
)

// Postgres is one independent PostgreSQL server. Tests keep their existing
// per-test schemas. Disruptive rehearsals own a separate runner/server.
type Postgres struct {
	DatabaseURL string
	ContainerID string
	container   testcontainers.Container
	docker      client.APIClient
}

// Start requires prebuilt, verified local images, binds a random loopback port,
// and returns only after a SQL query succeeds through that actual TCP endpoint.
func Start(ctx context.Context) (*Postgres, error) {
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return nil, fmt.Errorf("docker provider: %w", err)
	}
	if provider.Config().Config.RyukDisabled {
		return nil, errors.Join(
			fmt.Errorf("FRY test dependencies require Ryuk backup cleanup; enable TESTCONTAINERS_RYUK_DISABLED=false"),
			provider.Close(),
		)
	}
	images, err := Check(ctx, provider.Client())
	if err != nil {
		return nil, errors.Join(err, provider.Close())
	}
	provider.SetClient(&localClient{APIClient: provider.Client(), images: images})
	c, err := provider.CreateContainer(ctx, postgresRequest(images["postgres"]))
	if err != nil && c == nil {
		return nil, errors.Join(fmt.Errorf("create PostgreSQL: %w", err), provider.Close())
	}
	fixture := &Postgres{
		ContainerID: c.GetContainerID(),
		container:   c,
		docker:      provider.Client(),
	}
	if err != nil {
		return nil, fixture.failedStart(ctx, fmt.Errorf("create PostgreSQL: %w", err))
	}
	if err := fixture.start(ctx); err != nil {
		return nil, fixture.failedStart(ctx, err)
	}
	return fixture, nil
}

func (p *Postgres) failedStart(ctx context.Context, err error) error {
	p.Logs(context.WithoutCancel(ctx), os.Stderr)
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return errors.Join(err, p.Close(cleanupCtx))
}

func postgresRequest(image string) testcontainers.ContainerRequest {
	return testcontainers.ContainerRequest{
		Image: image, ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{"POSTGRES_PASSWORD": "fry-test-only", "POSTGRES_DB": "fry_test"},
		HostConfigModifier: func(config *container.HostConfig) {
			config.PortBindings = network.PortMap{
				network.MustParsePort("5432/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1")}},
			}
		},
	}
}

func (p *Postgres) start(ctx context.Context) error {
	if err := p.container.Start(ctx); err != nil {
		return fmt.Errorf("start PostgreSQL: %w", err)
	}
	port, err := p.container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return fmt.Errorf("PostgreSQL port: %w", err)
	}
	p.DatabaseURL = "postgres://postgres:fry-test-only@" + net.JoinHostPort(
		"127.0.0.1",
		port.Port(),
	) + "/fry_test?sslmode=disable"
	return waitForSQL(ctx, p.DatabaseURL)
}

func waitForSQL(ctx context.Context, url string) error {
	readyCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		lastErr = queryReady(readyCtx, url)
		if lastErr == nil {
			return nil
		}
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("PostgreSQL TCP/SQL readiness: %w", errors.Join(readyCtx.Err(), lastErr))
		case <-ticker.C:
		}
	}
}

func queryReady(ctx context.Context, url string) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	var value int
	queryErr := conn.QueryRow(ctx, "SELECT 1").Scan(&value)
	return errors.Join(queryErr, conn.Close(ctx))
}

// Close synchronously forces removal (including a paused server), removes
// anonymous volumes, and checks absence. Already removed containers are valid.
func (p *Postgres) Close(ctx context.Context) error {
	_, removeErr := p.docker.ContainerRemove(
		ctx,
		p.ContainerID,
		client.ContainerRemoveOptions{Force: true, RemoveVolumes: true},
	)
	if errdefs.IsNotFound(removeErr) {
		removeErr = nil
	}
	terminateErr := p.container.Terminate(ctx)
	if errdefs.IsNotFound(terminateErr) {
		terminateErr = nil
	}
	_, inspectErr := p.docker.ContainerInspect(ctx, p.ContainerID, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(inspectErr) {
		inspectErr = nil
	} else if inspectErr == nil {
		inspectErr = fmt.Errorf("PostgreSQL container %s still exists after cleanup", p.ContainerID)
	}
	return errors.Join(removeErr, terminateErr, inspectErr, p.docker.Close())
}

// Logs prints bounded diagnostics without replacing the original test failure.
func (p *Postgres) Logs(ctx context.Context, out io.Writer) {
	diagnosticCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	reader, err := p.container.Logs(diagnosticCtx)
	if err != nil {
		report(out, "PostgreSQL logs: %v\n", err)
		return
	}
	if _, err := io.Copy(out, io.LimitReader(reader, 128<<10)); err != nil {
		report(out, "Read PostgreSQL logs: %v\n", err)
	}
	if err := reader.Close(); err != nil {
		report(out, "Close PostgreSQL logs: %v\n", err)
	}
}

func report(out io.Writer, format string, args ...any) {
	// Diagnostics cannot be recovered if the caller's output stream is closed.
	//nolint:errcheck // Best effort diagnostics; lifecycle errors are returned separately.
	fmt.Fprintf(out, format, args...)
}
