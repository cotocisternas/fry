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
The complete engineering-foundation PR/release gates remain separate work.
Execution evidence and limits are recorded in
[native tooling validation](validation/native-tooling.md).

## Setup

Use Linux amd64 or arm64 with mise, Bash, Make, coreutils and Docker daemon
access. Native race tests and source builds also need the compiler/build
prerequisites reported by `make doctor`. Python and OpenSSL are used by process
rehearsals. No script changes system configuration or installs OS packages.

```sh
make setup-tools       # install pinned native tools and locked generator dependencies
make test-deps-build   # build FRY's PostgreSQL and cleanup-helper images locally
# make setup performs these two steps in order
make doctor
```

`mise.toml` and `mise.lock` select native tools. Go generator modules and npm
packages retain their respective committed dependency graphs under `tooling/rpc`.
Setup is the explicit download/install step. Normal commands select the prepared
tools without automatically installing a missing tool or upgrading it. Mise shell
activation is optional; wrappers establish the environment for each invocation.
Use the same Make commands in CI rather than independently reproducing recipes.

Test dependency images are built from repository Dockerfiles and pinned sources.
Pinned Linux base images and verified build dependencies may be downloaded during
explicit image builds. Test execution requires the resulting local images and
must not pull prebuilt PostgreSQL or cleanup-helper images. See
`tooling/testdeps` for the source pins, image identities and lifecycle code.

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
compatibility and native arm64 release execution remain independent gates.

`make check`, `make release-check`, `make security`, `make test-browser` and
`make proto-check` continue to fail explicitly until their broader gates are
implemented. `proto-drift-check` verifies current schema lint/build and generated
output; it does not claim compatibility with a released API version.
