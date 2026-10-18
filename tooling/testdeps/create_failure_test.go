package testdeps_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

func TestRyukFailureRemovesCreatedPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	docker, _ := checkedDocker(t, ctx)
	probe := &daemonProbe{rejectRyuk: true}
	environment := probe.serve(t)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cleanupCancel()
		for _, id := range probe.containers() {
			if _, err := docker.ContainerRemove(
				cleanupCtx,
				id,
				client.ContainerRemoveOptions{Force: true, RemoveVolumes: true},
			); err != nil &&
				!errdefs.IsNotFound(err) {
				t.Error(err)
			}
		}
	})
	command := exec.CommandContext(ctx, "../../scripts/test-deps.sh", "run", "--", "true")
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
		t.Fatalf("Ryuk failure status: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "deliberate Ryuk creation failure") {
		t.Fatalf("missing original failure: %s", output)
	}
	created := probe.containers()
	if len(created) != 1 {
		t.Fatalf("created %d containers, want the PostgreSQL server created before Ryuk", len(created))
	}
	assertRemoved(t, ctx, created[0])
}
