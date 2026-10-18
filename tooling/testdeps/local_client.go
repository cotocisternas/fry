package testdeps

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
)

// Testcontainers v0.44.0 hardcodes the Ryuk image name. Substitution at the
// Docker client boundary preserves its reaper detection while using our image
// by immutable local ID. No service, helper or SSHD image can be pulled.
type localClient struct {
	client.APIClient
	images map[string]string
}

func (c *localClient) localImage(name string) (string, error) {
	// v0.44.0 exposes no supported replacement; this value must track its internal reaper detection.
	//nolint:staticcheck // Reviewed call; rationale above.
	if name == testcontainers.ReaperDefaultImage {
		return c.images["ryuk"], nil
	}
	for _, id := range c.images {
		if name == id {
			return id, nil
		}
	}
	return "", fmt.Errorf("image %q is not a verified FRY dependency image", name)
}

func (c *localClient) ImageInspect(
	ctx context.Context,
	name string,
	opts ...client.ImageInspectOption,
) (client.ImageInspectResult, error) {
	id, err := c.localImage(name)
	if err != nil {
		return client.ImageInspectResult{}, err
	}
	return c.APIClient.ImageInspect(ctx, id, opts...)
}

func (c *localClient) ContainerCreate(
	ctx context.Context,
	opts client.ContainerCreateOptions,
) (client.ContainerCreateResult, error) {
	if opts.Config == nil {
		return client.ContainerCreateResult{}, fmt.Errorf("missing container config")
	}
	id, err := c.localImage(opts.Config.Image)
	if err != nil {
		return client.ContainerCreateResult{}, err
	}
	opts.Config.Image = id
	return c.APIClient.ContainerCreate(ctx, opts)
}

func (c *localClient) ImagePull(
	_ context.Context,
	name string,
	_ client.ImagePullOptions,
) (client.ImagePullResponse, error) {
	return nil, fmt.Errorf("runtime image pulls are forbidden (%s); run scripts/test-deps.sh pull", name)
}
