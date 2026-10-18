# Script organization validation

The initial 2026-10-18 cleanup introduced six root commands and grouped their
implementations by responsibility. The [script guide](../../scripts/README.md)
is the current command reference, including the seventh root command,
`scripts/bench.sh`, and the Python module implementations. The checks below
record the initial cleanup; they have not been rerun for those later changes.
Old flat paths were removed without compatibility wrappers.

The smoke launcher replaces nine repeated wrappers, validates the scenario and
arguments before entering the toolchain, and runs the existing scenario with its
own cleanup behavior. Python scenarios use package imports instead of loading
files under synthetic module names. Importing the replica scenario no longer
starts processes or installs signal handlers. The package-discovery regression
checks that imports do not start subprocesses, create sockets/directories, or
install signal handlers.

Shared fixture lifetime remains owned by the Go Testcontainers runner and its
shell/Python adapters. The launcher does not introduce a second cleanup layer.
Python bytecode generation is disabled through the native wrapper, and old
checkout bytecode was removed. Source-hash report inputs refer to actual moved
files and the shared launcher.

Review also exposed stale member-storage report and CLI assumptions from an
earlier transport refactor. Its source-hash input now names
`internal/admintransport/http.go`. Its checks for absent optional protobuf fields
accept omission as well as null, while still rejecting false or empty strings
where an unknown value is required. Checks for sampled false values remain
strict.

## Local checks

Passed on Linux amd64 with native Go 1.27.2 and repository-built PostgreSQL 18.6:

- `make doctor generate-check sqlc-vet` and `make fmt-check lint`.
- Bash syntax and ShellCheck 0.11.0 for every shell script, plus Node syntax
  checks for both relocated browser scripts. One local ShellCheck annotation
  documents the existing load-script function invoked through an EXIT trap.
- `make test-script-processes lint-policy-check`: safe package imports,
  timeout-resistant descendant cleanup, normal-exit cleanup and lint enforcement.
- Launcher help and invalid-input cases from outside the checkout; Make help.
- `./scripts/smoke.sh smtp`, `replicas`, `offline-restore`, `schema-startup`
  and `member-storage`.
  These preserve mail bytes, independent replica operations, source destruction
  before restore, and rejection of incompatible schemas without mutating them.
- The relocated receipt runner, invoked outside the checkout with the focused
  real-PostgreSQL race test
  `TestInstallationComposesAdministrationDirectoryAndReceipts`.
- `scripts/bench/hotstore-retained.sh` with 1,024 records of 1,024 bytes, including
  report publication and source hashing.
- Active-reference scans for every moved/deleted path and local documentation
  links. Existing validation outcomes remain historical; moved links do not
  imply that every older rehearsal was repeated.

Execution logs and the pre-change snapshot are under
`.artifacts/scripts-cleanup/`. Independent shell and Python reviews checked
argument flow, path resolution, import lifetimes and cleanup against that
snapshot; their stale-path findings were corrected. This pass did not rerun browser interactions,
container/kind deployments, every benchmark, the full Go suite, native arm64, or
live AWS validation. No application runtime behavior or dependency-image recipe
was changed.
