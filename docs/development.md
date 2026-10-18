# Development tooling

FRY uses mise-managed native tools for development and CI. Make remains the
command interface. Docker runs disposable test dependencies and explicit
container/deployment rehearsals; ordinary Go, lint, SQL, protobuf and browser
commands run on the host.

This workflow implements the accepted [quality policy](research/go-quality-policy.md).
Start with [contributor guidance](../CONTRIBUTING.md) for change design, code
ownership and choosing tests. A tool result supports review; it does not decide
whether a module is clear or an abstraction is useful.
The [script guide](../scripts/README.md) describes the small set of root launchers,
scenario names, module invocations and script directory responsibilities.
The contributor CI gate is `make check`; release qualification remains separate.
Execution evidence and limits are recorded in
[native tooling validation](validation/native-tooling.md) and the
[clean-clone onboarding record](validation/contributor-onboarding.md).

## Setup

Use a Linux amd64 or arm64 checkout with these host prerequisites already installed:

- Git, mise with support for the committed lockfile, Bash and GNU Make.
- GNU coreutils, `awk`, `cmp`, `diff`, `find`, `sort`, `xargs`, `tar` and `curl`.
  `make doctor` checks the required `sha256sum`, `realpath` and `timeout` too.
- Python 3, OpenSSL and a C compiler available as `cc` or through `CC`.
  The doctor checks these even for a Go-only onboarding run; race/cgo tests need
  the compiler, and Python/OpenSSL support generation and process rehearsals.
- The Docker CLI and an accessible Docker daemon for the demo, integration tests
  and source-built dependency images. `docker info` must succeed as your user.

Install host packages and mise before running setup. Normal setup does not
install OS packages or configure the Docker daemon. The explicit
`make browser-setup BROWSER_SETUP_FLAGS=--with-deps` command described below
also installs Playwright's Linux libraries and may require administrative access.
Authenticate to GitHub and obtain access to the private `cotocisternas/fry`
repository before cloning.

Run these commands from the checkout in Bash:

```bash
make setup-tools       # install pinned native tools and locked generator dependencies
source tooling/testdeps/images/pins.env
docker pull "$FRY_TESTDEPS_BASE"  # explicitly provision the permitted Linux base
make test-deps-build   # build FRY's PostgreSQL and cleanup-helper images locally
# Once the base is present, make setup combines setup-tools and test-deps-build.
make doctor
make build
make generate-check
make test-deps-check
make demo              # prints a localhost URL and synthetic credentials; Ctrl-C cleans up
```

[`mise.toml`](../mise.toml) and [`mise.lock`](../mise.lock) pin Go **1.27.2**,
Node **26.12.0** and Buf **1.74.0** for Linux amd64 and arm64. Golangci-lint
**2.15.0** and sqlc **1.31.1** are built locally with that Go compiler using the
committed module graphs under [`tooling/native`](../tooling/native/).
Go generator modules and npm packages retain their committed dependency graphs
under [`tooling/rpc`](../tooling/rpc/).
Setup is the explicit download/install step. Normal commands select the prepared
tools without automatically installing a missing tool or upgrading it. Mise shell
activation is optional; wrappers establish the environment for each invocation.
Use the same Make commands in CI rather than independently reproducing recipes.

Test dependency images are built from repository Dockerfiles and pinned sources.
The image builder requires the exact `FRY_TESTDEPS_BASE` from
[`pins.env`](../tooling/testdeps/images/pins.env) to exist locally; it fails with
`Pinned Linux base unavailable locally` otherwise. The explicit `docker pull`
above provisions that digest-pinned Debian base. PostgreSQL **18.6** is compiled
inside it using authenticated snapshot packages; Ryuk is compiled from its pinned
source commit into a scratch image. Explicit builds can download verified source
archives and build dependencies. Test execution accepts only the resulting local,
recipe-labelled images and forbids image pulls. See the
[fixture lifecycle reference](../tooling/testdeps/README.md) for source pins,
image identities, provenance checks and cleanup behavior.

Native generator binaries and npm dependencies live under the ignored
`.artifacts/native-toolchain/` in each checkout. Mise installations, Go caches and
verified test-image build inputs may be reused from host caches; copying another
checkout's `.artifacts` is unnecessary. Rerun `make setup-tools` after a tool or
lockfile change when a command reports stale or missing prepared inputs.
For `local ... image unavailable`, run `make test-deps-build` with the pinned
base present. For an occupied demo port, use `FRY_DEMO_HTTP_PORT=8081 make demo`.

For host-only compilation/generation, `make setup-tools doctor build generate-check`
does not need Docker. The demo and required integration checks do.

Valkey is not currently a FRY dependency. A future service must add its own
repository-owned image recipe and fixture when the corresponding feature needs it.

## Daily commands

```sh
make build
make fmt-check lint vet
make lint-policy-check         # prove naming/line-length/integer-range enforcement
make fmt                       # explicitly rewrites FRY-owned formatting
make generate-check sqlc-vet    # compares outputs without rewriting them
make generate                  # regenerates SQL, Go/TS RPC clients and browser bundle
make native-tools-check        # generation/ownership regressions with Docker unavailable
make test-scripts              # Docker-free Python script regressions
make test                      # lightweight run; PostgreSQL cases may skip
make test-race TEST_FLAGS=-short
make baseline-check            # complete existing-code baseline, not full release gate
make check                     # contributor CI gate after all prerequisites are prepared
```

`fmt` and `fmt-check` protect upstream fork sources from stylistic rewrites.
Lint includes root handwritten production and test code, including complexity,
error handling and resource ownership. Generated code is still compiled, vetted,
reproduced and exercised through integrations.
Receiver names must be consistent across a type's methods. Handwritten Go has a
120-column limit (tabs count as four), with automatic wrapping through `make fmt`.
Integer-range loops are required where the conversion preserves behavior;
loop-counter mutation and bounds that change during iteration require care.
These rules apply to tests and the separate dependency-tooling module too.
Duplication checks use a 100-token threshold. Review matches for shared meaning
before extracting code: preserve transaction ownership, lock lifetimes and
independent test expectations. Selected correctness checks, helper-entry checks,
error naming and built-in shadowing checks also run. Repeated literal reports
remain a review aid; unrelated concepts should not share a constant just because
their spelling matches.
See the [style-policy validation record](validation/lint-style.md) for the
cleanup, regression checks and integrated results.

The PostgreSQL integration runner uses Testcontainers-Go to start a fresh local
cluster, wait for readiness, export its endpoint and run the native test process.
One invocation shares that cluster across packages; tests use isolated schemas.
Tests that disrupt or destroy a whole cluster use separate fixture sessions.
Go package helpers alone cannot share a process-wide container across `go test
./...`, because packages execute in separate test binaries.

The runner owns cleanup and diagnostic logs, including on failure and interruption.
Required integration commands fail when Docker or the locally built images are
unavailable. A skipped database test in the lightweight command is not integration
verification. No test command should connect to a production database.

```sh
make test-integration
make test-race
make test-forks TEST_FLAGS=-race
make coverage TEST_FLAGS=-short
FRY_TEST_RUN='Migration|Import' FRY_TEST_PACKAGES='./internal/receiptstore ./internal/imapmigrate' \
  ./scripts/tests/receipts.sh -race -short
```

`FRY_TEST_RUN`, `FRY_TEST_PACKAGES` and `TEST_FLAGS` support focused investigation.
`FRY_TEST_TIMEOUT` controls package timeouts. Filtered runs are partial evidence.
The baseline runner rejects selection overrides. `-short` omits expensive large
message cases; run relevant large-message cases separately and identify the flag
in validation reports. Root `./...` does not run the two patched mail modules;
Make's integration/race targets explicitly run both complete suites.

Coverage reports separate handwritten and generated statements. Reviewed
per-package floors are in `tooling/coverage-floors.json`; an implementation change
must not silently lower them. Tooling modules are checked separately from FRY
application coverage.

## Dependency and process rehearsals

`make test-deps-check` checks local image availability and recipe metadata.
`make test-deps-test` runs the fixture lifecycle regressions under the race
detector; `make test-deps-lint` runs strict lint and vet on that separate module.
`make test-scripts` runs Python unittest discovery under `scripts/tests` through
the repository toolchain without Docker. `make test-script-processes` remains
the focused subset for command timeouts and descendant cleanup; that subset is
included in `make baseline-check` alongside the dependency checks.
The shared runner is also used by mail smoke tests, the disposable administration demo and
load/measurement scripts. Python restore rehearsals use dedicated fixture
sessions so source destruction remains a real assertion.

Use Make for the demo and smoke launcher:

```sh
make demo
make smoke SCENARIO=smtp
make smoke SCENARIO=offline-restore
# After explicitly building the application image:
make smoke SCENARIO=containers IMAGE=fry:local-test
./scripts/smoke.sh --help
# Separate opt-in measurements:
./scripts/bench.sh --help
./scripts/bench.sh load-mail 1000 65536 16 /tmp/fry-load-NEW.json
```

`make demo` invokes the `scripts.dev.demo_admin` Python module through the
repository toolchain. The public smoke launcher keeps the same scenario names;
the SMTP implementation is `scripts.smoke.smtp`. Measurements share
`scripts/bench.sh CASE [args]` and retain their existing arguments and defaults.

The daily Make surface is small. Failure rehearsals and benchmarks remain
available because they assert different contracts, and are selected for the
change rather than all run daily. Shell selects tools and orchestrates short
commands; configuration, report, protocol and process logic lives in Python
modules. See the [language ownership standard](../CONTRIBUTING.md#respect-source-ownership-and-explain-the-change).

Only `containers` and `kubernetes` accept an image argument; the default is
`fry:local-test`. Other scenarios accept no positional arguments. Scenario
prerequisites and the separate opt-in AWS probe are in the
[script guide](../scripts/README.md).

Container and Kubernetes rehearsals intentionally still run FRY in containers.
They consume the same locally built PostgreSQL image. Building a runtime image,
creating a local cluster or running a load rehearsal is an explicit action, not
part of formatting or code generation. These checks do not provision AWS or
establish multi-zone durability and production scale.

## Generation and tool upgrades

`sqlc.yaml` is the single SQL generation configuration. Runtime schemas are the
same files consumed by sqlc under `db/schema`; queries are under `db/queries`.
Generated query code remains in each owning domain's `dbgen` directory.

Administration and member protobuf inputs are isolated under `proto/admin` and
`proto/member`, with generated Go/TS output under `gen/admin` and `gen/member`.
The [RPC tooling reference](../tooling/rpc/README.md) describes local generators,
output ownership and reproducibility checks. Native generation retains scratch
staging and output guards; it is not a container security sandbox.

Upgrade native pins and their lock entries together. Review upstream release
and checksum evidence, regenerate through the explicit setup path, and rerun
lint, tests and generation checks. Upgrade image source/base pins through an
explicit local rebuild and fixture regression run. Existing published-version
compatibility and native arm64 release execution remain independent evidence.

`make proto-drift-check` verifies current schema lint/build and generated output.
`make proto-check` adds Buf FILE compatibility against the explicit immutable
initial administration baseline and against the PR base when supplied. This
establishes compatibility with those inputs; it does not certify an unrecorded
released API. The initial SHA is in
[`tooling/rpc/admin-baseline.txt`](../tooling/rpc/admin-baseline.txt); the guard is
a deliberate development-source baseline. `FRY_PROTO_BASE_REF` accepts a full
local commit SHA for an additional comparison. CI supplies the PR base and
fetches history; local shallow checkouts need those commits available too.
[`verify-compatibility.sh`](../tooling/rpc/verify-compatibility.sh) proves that
the policy rejects a removed RPC and accepts an additive message.

## CI and local verification

The [GitHub Actions contributor workflow](../.github/workflows/ci.yml) runs on
pull requests, pushes to `main` and manual dispatch, on Ubuntu 24.04 Linux amd64
with pinned checkout and mise setup. After the native tools, pinned base and
source-built images above are prepared, run:

```sh
make security-setup
make browser-setup
make check
```

`make security-setup` builds the pinned scanners from the committed Go module
graph under `tooling/security/scanners`. `make browser-setup` installs the locked
Playwright package from `scripts/browser/package-lock.json` and its Chromium
headless shell under `.artifacts/browser/`. Default browser setup does not
install OS packages. On Ubuntu 24.04, CI uses
`make browser-setup BROWSER_SETUP_FLAGS=--with-deps` to explicitly install the
Linux libraries too. On other distributions, prepare equivalent libraries with
the distribution's package manager. Missing browser prerequisites fail the gate.

The PostgreSQL and Ryuk images are source-built on the runner. No service
credentials or production database are needed.

`make check` orchestrates the existing-code checks, browser, security and
protobuf checks through [`scripts/tests/check.sh`](../scripts/tests/check.sh).
It includes strict formatting/lint, command builds,
root and fork vet, generation reproducibility, SQL checks, fixture lifecycle
tests, process regressions, root PostgreSQL race/coverage tests with `-short`,
reviewed coverage floors and both complete patched mail-library race suites.
`make test-browser` runs both administration and member-storage Chromium suites
against an owned disposable demo. `make security` uses the prepared scanners;
lint alone is not its substitute. Ordinary browser/security checks do not install
missing scanner or browser binaries. Security checks may fetch current advisory
data, so they require network access even after scanner setup.
Successful contributor-gate evidence is written under
`.artifacts/contributor-checks/`, including source hashes, coverage output and
the final `PASSED` marker; failures remove the old success marker.
Select individual targets for focused local work and identify their limits.

The repository remains private. On 2026-10-18, GitHub's branch-protection API
returned HTTP 403 requiring GitHub Pro or a public repository. With the current
plan, Actions checks can run but cannot be enforced as required merge checks.
Review check results before merging; do not interpret the workflow as an enforced
branch-protection rule.

`make release-check` remains unavailable: native arm64 execution and the broader
release/recovery rehearsals need their own evidence. A passing local check does
not establish a hosted Actions result. The
[onboarding record](validation/contributor-onboarding.md) states the exact
committed baseline and commands independently executed in a fresh checkout.
