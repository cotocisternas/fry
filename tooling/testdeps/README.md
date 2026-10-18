# Native test dependency lifecycle

This separate Go module owns FRY's custom Testcontainers fixtures and keeps
Docker dependencies out of the production module. It uses the generic Docker
provider, with FRY-owned PostgreSQL startup, TCP/SQL readiness and checked
cleanup. Service images come from upstream; there is no image build framework
or Testcontainers PostgreSQL service module.

The module's `go 1.27.2` directive matches the pinned native toolchain.
`scripts/setup.sh` downloads and verifies its Go dependencies. The wrapper
resolves that compiler through `scripts/toolchain.sh go`; every command uses
`GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local`. Missing Go dependencies require
setup. Cached CLI binaries live outside the checkout under
`${XDG_CACHE_HOME:-$HOME/.cache}/fry/testdeps/bin`.

## Commands

```sh
make test-deps-pull      # Explicitly download the pinned upstream images
make test-deps-check     # Verify local images without pulling
make test-deps-test
make test-deps-lint
scripts/test-deps.sh image postgres
scripts/test-deps.sh image ryuk
scripts/test-deps.sh run -- scripts/toolchain.sh test-go test ./...
scripts/test-deps.sh vet
scripts/test-deps.sh fmt-check
```

`make setup` prepares the native tools and pulls the images. The direct
`scripts/test-deps.sh pull` command selects `linux/<host architecture>`, has a
ten-minute deadline, and checks the resulting local images. It only downloads
those two reviewed images.

`image postgres` and `image ryuk` print exactly one
`docker.io/REPOSITORY:VERSION@sha256:DIGEST` reference. They do not contact
Docker or require an image to exist locally. The pins in [images.go](images.go)
are the source of truth for shell, Python and deployment consumers.

`check` inspects both pinned references, verifies their `RepoDigests` and
Linux/host architecture, and resolves local immutable image IDs. Docker Hub
prefixes (`docker.io/` and `library/`) are normalized for comparison. Both
images must pass before `run` creates containers. Its Docker client guard
allows creation only from those IDs and rejects runtime pulls, including
Testcontainers helper pulls. A missing image requires explicit `pull`, including
if it disappears between inspection and creation. Ryuk must remain enabled.

## Fixture behavior

`run -- COMMAND ARGS` starts an independent PostgreSQL server, publishes a
random port on numeric `127.0.0.1`, and checks `SELECT 1` through that actual
TCP endpoint with pgx before starting the child. It exports:

- `FRY_TEST_DATABASE_URL=postgres://postgres:fry-test-only@127.0.0.1:PORT/fry_test?sslmode=disable`
- `FRY_TEST_POSTGRES_CONTAINER`, the full container ID for `docker exec`
  operations such as `psql`, `createdb`, `pg_dump`, and `pg_restore`.

Each CLI invocation uses a fresh Testcontainers session ID so sibling runners
own independent servers and reapers. Calls to the Go `Start` API in one process
share its reaper while owning separate PostgreSQL containers. Existing test
schemas and the shell/Python session API under `scripts/lib/` remain unchanged.
See the [script guide](../../scripts/README.md) for those consumers.

The child inherits stdin/stdout/stderr and preserves its exit status.
Cancellation sends its process group SIGTERM, then SIGKILL after the bounded
wait. Ordinary completion also kills remaining descendants. Startup and child
failures emit bounded logs. Cleanup uses a fresh 30-second context, removes
paused containers and anonymous volumes, and checks absence. Already removed
containers are accepted; cleanup errors make an otherwise successful run fail.
Ryuk provides backup cleanup if the process is lost.

The official PostgreSQL 18 image uses
`PGDATA=/var/lib/postgresql/18/docker` and declares the parent volume
`/var/lib/postgresql`. Bind mounts, tmpfs mounts and deployment PVCs must target
that parent. Its default entrypoint starts as root, prepares ownership and
then drops to OS `postgres`, UID/GID **999**. Deployments that set user 999
explicitly must provide a writable volume. Its Unix socket directory is
`/var/run/postgresql`. PostgreSQL reports a Debian package suffix in
`server_version`; `server_version_num` is **180006**.

## Pins and upgrades

Verified against primary sources on **2026-10-18**:

- PostgreSQL **18.6-trixie**, the variant also assigned `latest` in the
  [official Docker image definitions](https://github.com/docker-library/official-images/blob/master/library/postgres).
  The [upstream Dockerfile](https://github.com/docker-library/postgres/blob/e00e1bd34ec5c8a8e7ad89b273b3d42efaf6d5bc/18/trixie/Dockerfile)
  and [entrypoint](https://github.com/docker-library/postgres/blob/e00e1bd34ec5c8a8e7ad89b273b3d42efaf6d5bc/docker-entrypoint.sh)
  define its runtime conventions.
- Testcontainers-Go **v0.44.0** requires upstream **testcontainers/ryuk:0.14.0**
  in its [reaper configuration](https://github.com/testcontainers/testcontainers-go/blob/v0.44.0/internal/config/config.go).
  The local-only client maps that hardcoded name to the checked immutable
  helper ID while preserving Testcontainers' reaper detection.
- Both OCI index digests were read from the Docker Hub registry's primary
  manifest endpoints (`registry-1.docker.io/v2/library/postgres/manifests/18.6-trixie`
  and `registry-1.docker.io/v2/testcontainers/ryuk/manifests/0.14.0`). Each
  `Docker-Content-Digest` header matched the SHA-256 of the manifest response.
  These multi-platform index pins fix the platform-specific manifests too.

To upgrade, verify upstream versions and registry manifests, update the two
references in `images.go`, explicitly pull, and run this module's lifecycle
tests, lint, format and vet checks plus the root integration baseline. Keep
Ryuk aligned with the pinned Testcontainers library. No source checksums,
Debian snapshots, provenance labels or custom image builds are involved.

The lifecycle suite covers TCP/SQL readiness, image identity and platform
rejection, no runtime pulls, explicit/paused/prior removal, cancellation during
startup, failed Ryuk creation, bounded failure diagnostics, child status,
descendant cleanup and independent CLI sessions. A recording proxy forwards
to the real Docker daemon for failure cases without deleting shared images.
Future services belong behind this same fixture boundary with pinned upstream
images, protocol readiness and checked cleanup; add them when a consumer needs
them. Valkey currently has no consumer.
