package testdeps

import (
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"runtime"

	"github.com/moby/moby/client"
)

//go:embed images/* go.mod
var recipes embed.FS

const recipeLabel = "io.fry.testdeps.recipe"

// Image identifies a FRY-built image by the complete reviewed recipe and architecture.
// It performs no Docker operations and never builds or pulls an image.
func Image(kind string) (tag, digest string, err error) {
	if kind != "postgres" && kind != "ryuk" {
		return "", "", fmt.Errorf("unknown dependency image %q", kind)
	}
	sum := sha256.New()
	err = fs.WalkDir(recipes, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, readErr := recipes.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if _, writeErr := fmt.Fprintf(sum, "%s\x00%s\x00", path, data); writeErr != nil {
			return writeErr
		}
		return nil
	})
	if err != nil {
		return "", "", fmt.Errorf("hash image recipe: %w", err)
	}
	digest = fmt.Sprintf("%x", sum.Sum(nil))
	return fmt.Sprintf("fry-testdeps-%s:%s-%s", kind, digest[:24], runtime.GOARCH), digest, nil
}

// Check verifies that both current recipe images exist locally and have matching
// provenance labels. A missing or stale image requires an explicit build.
func Check(ctx context.Context, docker client.APIClient) (map[string]string, error) {
	images := make(map[string]string)
	for _, kind := range []string{"postgres", "ryuk"} {
		tag, digest, err := Image(kind)
		if err != nil {
			return nil, err
		}
		inspected, err := docker.ImageInspect(ctx, tag)
		if err != nil {
			return nil, fmt.Errorf("local %s image unavailable; run scripts/test-deps.sh build: %w", kind, err)
		}
		if inspected.Config == nil || inspected.Config.Labels[recipeLabel] != digest ||
			inspected.Config.Labels["io.fry.testdeps.kind"] != kind {
			return nil, fmt.Errorf("local image %s has invalid recipe metadata; run scripts/test-deps.sh build", tag)
		}
		if inspected.Os != "linux" || inspected.Architecture != runtime.GOARCH {
			return nil, fmt.Errorf("local image %s has wrong platform", tag)
		}
		images[kind] = inspected.ID
	}
	return images, nil
}
