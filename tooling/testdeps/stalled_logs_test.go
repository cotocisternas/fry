package testdeps_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

func TestStalledStartupLogsDoNotConsumeCleanupBudget(t *testing.T) {
	assertCleanupAfterStalledLogs(t, &daemonProbe{rejectRyuk: true, stallLogs: true}, []string{"true"}, 1)
}

func TestStalledChildFailureLogsDoNotConsumeCleanupBudget(t *testing.T) {
	assertCleanupAfterStalledLogs(t, &daemonProbe{stallLogs: true}, []string{"sh", "-c", "exit 37"}, 37)
}

func assertCleanupAfterStalledLogs(t *testing.T, probe *daemonProbe, args []string, expectedStatus int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	docker, _ := checkedDocker(t, ctx)
	environment := probe.serve(t)
	t.Cleanup(func() { removeProbedContainers(t, ctx, docker, probe) })
	// Owner: FRY tooling; arguments are fixed regression commands, not external input. Revisit 2027-10-18.
	//nolint:gosec // Reviewed call; rationale above.
	command := exec.CommandContext(ctx, "../../scripts/test-deps.sh", append([]string{"run", "--"}, args...)...)
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != expectedStatus {
		t.Fatalf("failure status: %v, want %d\n%s", err, expectedStatus, output)
	}
	logs, deletes := probe.cleanupTraffic()
	if logs == 0 {
		t.Fatal("regression did not exercise stalled logs")
	}
	if deletes == 0 {
		t.Fatalf("stalled logs prevented PostgreSQL deletion\n%s", output)
	}
	created := probe.containers()
	if len(created) == 0 {
		t.Fatal("regression did not create PostgreSQL")
	}
	assertRemoved(t, ctx, created[0])
}

func removeProbedContainers(t *testing.T, ctx context.Context, docker client.APIClient, probe *daemonProbe) {
	t.Helper()
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	for _, id := range probe.containers() {
		_, err := docker.ContainerRemove(
			cleanupCtx,
			id,
			client.ContainerRemoveOptions{Force: true, RemoveVolumes: true},
		)
		if err != nil && !errdefs.IsNotFound(err) {
			t.Error(err)
		}
	}
}
