package testdeps

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/moby/moby/client"
)

// Image returns a reviewed upstream image reference without contacting Docker.
// The OCI index digest fixes the contents; Docker selects the host architecture.
func Image(kind string) (string, error) {
	switch kind {
	case "postgres":
		return "docker.io/library/postgres:18.6-trixie@" +
			"sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336", nil
	case "ryuk":
		return "docker.io/testcontainers/ryuk:0.14.0@" +
			"sha256:7c1a8a9a47c780ed0f983770a662f80deb115d95cce3e2daa3d12115b8cd28f0", nil
	default:
		return "", fmt.Errorf("unknown dependency image %q", kind)
	}
}

// Check requires the pinned images locally and resolves immutable runtime IDs.
// It never pulls images; setup must explicitly run scripts/test-deps.sh pull.
func Check(ctx context.Context, docker client.APIClient) (map[string]string, error) {
	images := make(map[string]string)
	for _, kind := range []string{"postgres", "ryuk"} {
		ref, err := Image(kind)
		if err != nil {
			return nil, err
		}
		inspected, err := docker.ImageInspect(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("local %s image unavailable; run scripts/test-deps.sh pull: %w", kind, err)
		}
		if !hasRepoDigest(inspected.RepoDigests, ref) {
			return nil, fmt.Errorf("local image %s has wrong RepoDigests; run scripts/test-deps.sh pull", ref)
		}
		if inspected.Os != "linux" || inspected.Architecture != runtime.GOARCH {
			return nil, fmt.Errorf(
				"local image %s must be linux/%s; run scripts/test-deps.sh pull",
				ref,
				runtime.GOARCH,
			)
		}
		images[kind] = inspected.ID
	}
	return images, nil
}

func hasRepoDigest(actual []string, ref string) bool {
	repository, digest, _ := strings.Cut(ref, "@")
	repository, _, _ = strings.Cut(repository, ":")
	want := normalizeRepoDigest(repository + "@" + digest)
	for _, candidate := range actual {
		if normalizeRepoDigest(candidate) == want {
			return true
		}
	}
	return false
}

func normalizeRepoDigest(ref string) string {
	return strings.TrimPrefix(strings.TrimPrefix(ref, "docker.io/"), "library/")
}
