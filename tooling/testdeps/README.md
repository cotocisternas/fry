# Native test dependency lifecycle

This separate Go module keeps Docker/Testcontainers dependencies out of FRY's
production module. Its `go 1.27.2` directive matches the pinned native toolchain;
the wrapper resolves the compiler through `scripts/toolchain.sh go` and checks
that exact version. `scripts/setup.sh` downloads and verifies this module's
graph. Runtime commands, tests and vet set `GOPROXY=off GOSUMDB=off` and
`GOTOOLCHAIN=local`; a missing cache is a setup failure, never a tool download.

## Commands

Run these from the checkout:

```sh
make test-deps-build
make test-deps-check
make test-deps-test
make test-deps-lint
# Direct commands for image identity, custom children and individual checks:
scripts/test-deps.sh image postgres
scripts/test-deps.sh image ryuk
scripts/test-deps.sh run -- scripts/toolchain.sh test-go test ./...
scripts/test-deps.sh lint
scripts/test-deps.sh vet
scripts/test-deps.sh fmt-check
```

The [script guide](../../scripts/README.md) explains the root launchers and the
shared shell/Python helpers under `scripts/lib/`.

Only `build` downloads verified sources/build dependencies or builds images.
It builds PostgreSQL inside a digest-pinned Debian base and compiles Ryuk
natively with the pinned Go compiler into a scratch image. Docker's built-in
builder avoids a separate BuildKit image. Download archives, build directories
and cached CLI binaries live under `${XDG_CACHE_HOME:-$HOME/.cache}/fry/testdeps`,
outside the source tree. Changing a recipe requires an explicit rebuild.

`image` only prints the current content-derived local reference. `check` performs
a fast local image inspection, checking both full recipe provenance labels and
Linux/host architecture. Both images must exist before `run` creates anything.
There is no automatic build, service image pull, Ryuk pull, or SSHD helper pull.
`run` additionally replaces Testcontainers' Docker client with a local-only
guard: only the checked immutable image IDs may create containers, and its pull
operation always fails. A missing image between inspection and creation also
fails. Keep Ryuk enabled; disabling it is rejected.

`run -- COMMAND ARGS` starts one independent server, publishes a random port on
numeric `127.0.0.1`, and checks `SELECT 1` through that TCP endpoint using pgx
before starting the command. It exports:

- `FRY_TEST_DATABASE_URL=postgres://postgres:fry-test-only@127.0.0.1:PORT/fry_test?sslmode=disable`
- `FRY_TEST_POSTGRES_CONTAINER`, the full container ID for existing `docker exec`
  operations such as `psql`, `createdb`, `pg_dump`, and `pg_restore`.

Every standalone runner uses a fresh Testcontainers session ID before provider
initialization. Testcontainers' default parent-PID session would otherwise join
independent siblings spawned by one shell/Python process and race during Ryuk
creation/removal. Calls to the Go `Start` API in one process intentionally share
that process's reaper while owning separate PostgreSQL containers.

The child inherits stdin/stdout/stderr and its exit status is preserved. On
cancellation, the child process group receives SIGTERM, followed by SIGKILL
after the bounded wait. Every child completion also kills remaining group
members, including descendants that detached their streams, before database
cleanup. A failed child/startup emits
PostgreSQL logs with a two-second deadline. Cleanup then receives a fresh
30-second context independent of diagnostics and test cancellation, forcibly
removes paused servers and anonymous volumes, and checks
container absence before success. Already removed containers are accepted;
measurement scripts may explicitly destroy their server before publishing
successful evidence. Cleanup errors make an otherwise successful run fail.
Ryuk remains a backup for process loss (its normal grace period means the
helper can briefly remain after the CLI exits).

The image runs as OS `postgres` UID/GID 999, with writable
`PGDATA=/var/lib/postgresql/data` and Unix socket `/tmp`. It accepts
`POSTGRES_USER` (default `postgres`), `POSTGRES_DB` (default `fry_test`) and
required `POSTGRES_PASSWORD`. Host authentication uses SCRAM; the deliberately
disposable fixture accepts bridge clients, while the runner's published port
is loopback-only. Fresh deployment volumes must be writable by UID/GID 999.
This is a test/deployment-smoke recipe, not a production PostgreSQL hardening
or upgrade policy.

## Integration and checks

`scripts/tests/receipts.sh` reenters through this runner once and keeps the
existing root-package, run-filter, timeout, coverage argument and `TMPDIR`
behavior. Root test schemas remain isolated as before. Root/fork suite selection
stays in the root quality adapters. Disruptive restore/schema rehearsals use a
dedicated runner per server, as implemented by `scripts/lib/testdeps.py` sessions;
its stdin holder keeps the runner alive until checked teardown. No new daemon,
session descriptor service or raw Docker startup loop is needed.

`test` runs this module's complete tests with `-race -count=1`, requiring local
images and Docker. Tests cover real TCP/SQL readiness, explicit removal,
paused/prior removal, cancellation during startup, failure creating Ryuk after
PostgreSQL already exists, child failure status and logs, descendant cleanup
after interruption and ordinary exit, and concurrent/sequential CLI ownership.
A local recording proxy forwards to the real Docker socket, checks actual
running PostgreSQL/Ryuk image IDs, rejects any attempted pull, and makes a local
image unavailable to prove preflight failure without mutation. Strict lint,
format and vet use the root policy separately from the production module.

## Verified pins and upgrade procedure

Verified against primary sources on 2026-10-18:

- Testcontainers-Go **v0.44.0** is the latest stable
  [upstream release](https://github.com/testcontainers/testcontainers-go/releases/tag/v0.44.0),
  confirmed through the GitHub latest-release API and
  [Go module proxy](https://proxy.golang.org/github.com/testcontainers/testcontainers-go/@latest).
  Its module is pinned and authenticated by `go.sum`.
- PostgreSQL **18.6** comes from the
  [official source directory](https://ftp.postgresql.org/pub/source/v18.6/), with
  SHA-256 `555610c24d53e4316da5b7d3fc25c279d96856d5e0e23ee308c328c5fa881d9f`
  checked against the [published checksum](https://ftp.postgresql.org/pub/source/v18.6/postgresql-18.6.tar.bz2.sha256).
- Ryuk **0.14.0**, the version expected by Testcontainers-Go v0.44.0, is built
  from [upstream commit b3726afd](https://github.com/testcontainers/moby-ryuk/tree/b3726afd6cc2c36628abcc08e9cabac43f587384).
  Its full archive hash and commit are in `images/pins.env`; the source's
  `go.mod`/`go.sum` pin build dependencies and `go mod verify` checks the cache.
  v0.44.0 hardcodes the image name, so the Docker client guard maps that name to
  our local image ID while preserving Testcontainers' reaper detection. No SDK
  fork or unsupported Ryuk-image environment variable is used.
- Debian **trixie-slim** is pinned by multi-platform OCI digest in `pins.env`,
  verified against the [official Debian image metadata](https://hub.docker.com/v2/repositories/library/debian/tags/trixie-slim).
  Both build and runtime stages use fixed **20261017T000000Z**
  [main](https://snapshot.debian.org/archive/debian/20261017T000000Z/)
  and [security](https://snapshot.debian.org/archive/debian-security/20261017T000000Z/)
  snapshots and upgrade installed packages before adding dependencies. APT uses
  the Debian archive keyring to authenticate each index and package checksum;
  only historical `Valid-Until` expiry is disabled. The security repository
  supplies OpenSSL **3.5.7-1~deb13u3**, including fixes unavailable in bookworm.
  The image builder requires the pinned Linux base to exist locally and never
  implicitly pulls it. Explicitly provision that exact `FRY_TESTDEPS_BASE`
  reference from `images/pins.env` before the first build. Service and helper
  images remain source-built. See [image remediation](../../docs/validation/image-remediation-2026-10-18.md)
  for build, scan and actual PostgreSQL lifecycle evidence.

On an upgrade, recheck primary release/checksum sources, update the module or
recipe pins, explicitly build on each supported host architecture, and run
this module's strict checks and real lifecycle suite plus the root integration
baseline. Both images carry `io.fry.testdeps.kind` and the full
`io.fry.testdeps.recipe` SHA-256; their tags contain its first 24 hex digits and
the architecture. The recipe hash covers all embedded image inputs and this
module's Go/tool pins, so provenance changes invalidate old references.

Future dependency types belong behind this same owned fixture boundary: add a
verified source recipe, local image preflight, actual protocol readiness, and
checked cleanup, then expose only the environment needed by their consumers.
Valkey is intentionally not implemented until a concrete consumer needs it.
