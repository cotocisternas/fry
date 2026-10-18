# Native development tools with mise

Researched and implemented 2026-10-18. Scope: native tool selection and explicit
setup; Make remains the command interface. Docker is reserved for disposable
service dependencies and explicit image/deployment checks. No system or personal
configuration was changed. Mise behavior was checked against its primary
documentation/source and the installed **2026.10.12** CLI.

Security remediation on the same date replaces the linter/sqlc release binaries
with verified repository-local source builds. See the
[security build research](native-tool-security-builds.md) and
[remediation validation](../validation/native-tool-security.md).

## Ownership and selected backends

[`mise.toml`](../../mise.toml) owns runtime/Buf versions, with explicit
backends rather than a potentially changing shorthand:

- `core:go`: **1.27.2**; `core:node`: **26.12.0**. Explicit `core:` bypasses
  installed plugin overrides. These installers use official runtime archives.
  [Core selection](https://mise.jdx.dev/core-tools.html#selecting-another-implementation),
  [Go](https://mise.jdx.dev/lang/go.html), [Node](https://mise.jdx.dev/lang/node.html).
- `aqua:bufbuild/buf`: **1.74.0**. Its recipe selects platform archives and publisher verification
  metadata.
  [Buf recipe](https://github.com/aquaproj/aqua-registry/blob/main/pkgs/bufbuild/buf/registry.yaml).
- golangci-lint **2.15.0** and sqlc **1.31.1** are built with mise's Go **1.27.2**
  from separate locked modules under [`tooling/native/`](../../tooling/native/).
  The linter retains upstream's dependency versions. sqlc selects patched
  dependencies; exact selections and provenance are recorded in those modules
  and per-executable build receipts. Their old mise release-binary entries were
  removed. [`tooling/versions.env`](../../tooling/versions.env) now records only
  the provenance schema, not the old official linter compiler requirement.

The Go generator graph remains owned by
[`tooling/rpc/tools/go.mod`](../../tooling/rpc/tools/go.mod) and its `go.sum`:
`google.golang.org/protobuf/cmd/protoc-gen-go` **v1.36.12** and
`connectrpc.com/connect/v2/cmd/protoc-gen-connect-go` **v2.0.1**. Setup builds them
with the pinned Go compiler using `-trimpath -buildvcs=false -mod=readonly`.
Their versions are not repeated in mise. Mise also supports
`go:google.golang.org/protobuf/cmd/protoc-gen-go` and
`go:connectrpc.com/connect/v2/cmd/protoc-gen-connect-go` with exact version pins,
but that backend invokes `go install package@version` and does not consume this
repository's generator module graph; its lock entries cover only the top-level
version. It is therefore not used here. [Go backend](https://mise.jdx.dev/dev-tools/backends/go.html),
[implementation](https://github.com/jdx/mise/blob/v2026.10.12/src/backend/go.rs),
[backend lock coverage](https://mise.jdx.dev/dev-tools/mise-lock.html#backend-support).

The existing npm manifest and version-3 lockfile remain the sole owners of
protobuf/protoc-gen-es **2.16.0**, Connect/Connect-Web **2.2.0**, esbuild
**0.28.2**, TypeScript **7.0.2**, and their transitive dependencies. The lock
contains the Linux x64 and arm64 optional binary packages. Preserve the nested
TypeScript **5.4.5** required by the generator; do not flatten that dependency.
No duplicate `npm:` tools or additional package-manager lockfile are needed.
The mise npm backend itself recommends using a project's existing manifest
when a CLI is already declared there. [npm backend](https://mise.jdx.dev/dev-tools/backends/npm.html).
Node 26.12.0 bundles npm **11.20.0**; no separate npm upgrade is introduced.
[Tagged npm manifest](https://github.com/nodejs/node/blob/v26.12.0/deps/npm/package.json).

## Installation and routine execution

`scripts/setup.sh` is the explicit installation/cache boundary. It runs
`mise install --locked`, downloads and verifies Go modules against disposable
copies, builds the two source CLIs and two Go generators, and installs the npm graph with
`npm ci --include=dev --include=optional --no-audit --no-fund`. Original module
files and npm lockfiles must remain byte-for-byte unchanged. `npm ci` rejects
manifest/lock disagreement and does not rewrite the lockfile; it is still an
installation operation and may need network access. [npm ci](https://docs.npmjs.com/cli/v11/commands/npm-ci).

[`mise.lock`](../../mise.lock) records versions, backends, artifact URLs and
checksums for **linux-x64** and **linux-arm64**. These are mise's platform names;
the former corresponds to Go/Docker amd64. Maintainers regenerate it explicitly
with `mise lock --platform linux-x64,linux-arm64`. Locked installation fails on
missing lock entries rather than re-resolving; it does not imply offline
installation or validate every byte of an already-installed tool cache.
[Lock semantics](https://mise.jdx.dev/dev-tools/mise-lock.html),
[platform keys](https://mise.jdx.dev/dev-tools/mise-lock-reference.html#platform-keys).

`scripts/toolchain.sh` provides `go`, `test-go`, `lint`, `sqlc`, `buf`, `exec`,
`receipts` and `versions`. It disables mise automatic installation, uses a
non-installing `mise install --locked --dry-run-code` preflight, verifies actual
binary versions, validates source CLI hashes/build metadata/manifest fingerprints,
then enters `mise exec --no-deps`. Shell activation is
unnecessary. The wrapper preserves the caller's working directory, including
nested fork modules, and prepends the exact validated executable directories
inside the child process. This matters because an activated shell's PATH can
otherwise select a system Node. [Exec behavior](https://mise.jdx.dev/cli/exec.html),
[preflight flag](https://mise.jdx.dev/cli/install.html),
[auto-install controls](https://mise.jdx.dev/configuration/settings.html#auto_install).

Generic `exec` also exports absolute `FRY_RPC_NODE_MODULES` and `FRY_RPC_BIN`,
and exposes both Go generators and npm `.bin` tools on PATH. Prepared tools live
under `.artifacts/native-toolchain/<host-architecture>/`; a fingerprint of the
committed inputs, including every prepared module manifest/checksum, rejects stale
preparation with an instruction to rerun setup. Source CLIs have separate
`*.build.json` receipts; every invocation checks their bytes, compiler, package,
release, embedded module versions/checksums and reviewed recipe inputs.
Missing tools never trigger setup. `GOTOOLCHAIN=local` prevents Go from downloading
another runtime, while `GOWORK=off` and `GOFLAGS=-mod=readonly` preserve existing
module behavior. Module downloads are a separate concern from runtime installs.
[Go toolchain selection](https://go.dev/doc/toolchain).

`doctor.sh` checks native tools, generator/npm readiness, Bash, Make, Python,
OpenSSL, the C compiler and supporting host commands. It never starts Docker or
checks service images. `receipts` hands off to `scripts/tests/receipts.sh`; the
service fixture owns its lifecycle. There is no mise task graph or custom
runtime downloader. The former compiler-tool Docker recipes and unused image
pins have been removed by the parent/native integration work.

## Testcontainers and future dependencies

Use native Go tests with shared Go fixture code around the official
[Postgres module](https://golang.testcontainers.org/modules/postgres/), consuming
the project's locally built, pinned PostgreSQL image. Keep unit tests independent
of Docker; the required integration gate must fail when the daemon or required
images are unavailable. Do not silently pull prebuilt service images.

Prefer one PostgreSQL instance/database per suite invocation and a unique schema
per test. Existing helpers already use `CREATE SCHEMA` and connection
`search_path`, for example
[`receiptstore_linux_test.go`](../../internal/receiptstore/receiptstore_linux_test.go).
Give restart, termination, configuration or other disruptive cases dedicated
instances. Use bounded readiness checks, retain diagnostic logs on failure, and
register cleanup immediately; Ryuk is a cleanup backstop.
[Postgres readiness](https://github.com/testcontainers/testcontainers-go/blob/main/modules/postgres/wait_strategies.go),
[logs](https://golang.testcontainers.org/features/follow_logs/),
[cleanup/lifecycle](https://golang.testcontainers.org/features/creating_container/),
[garbage collection](https://golang.testcontainers.org/features/garbage_collector/).

Go test packages run in separate processes. A package helper or `sync.Once` does
not share one database across `go test ./...`; global sharing requires an explicit
parent runner that starts the fixture, passes the DSN, waits for all test
processes, and cleans up. A shared Testcontainers session ID groups resources,
not application container instances.
[Official session semantics](https://golang.testcontainers.org/features/test_session_semantics/).
Use the official [Valkey module](https://golang.testcontainers.org/modules/valkey/)
and the same lifecycle contract only when a feature actually requires Valkey;
do not introduce speculative service dependencies.

## Validation and remaining limits

- Original conversion evidence: explicit setup and repeated setup passed on Linux amd64 with mise 2026.10.12.
  All module manifests/checksums and npm/config lockfiles remained unchanged.
  All ten then-selected runtime/CLI archive checksums matched primary publisher metadata;
  [local evidence](../../.artifacts/native-toolchain/checksums.json).
- Doctor, Bash syntax and ShellCheck 0.11 checks passed. Missing installations,
  mismatched binaries, stale prepared inputs and the removed Docker toolchain
  mode fail without implicit repair/install;
  [negative checks](../../.artifacts/native-toolchain/negative-checks.json).
- Root and both fork module identities were checked through the dispatcher;
  a focused root Go package test passed. Parent integration reports native
  build/format and SQL/proto/browser/fixture checks passing with Docker
  unavailable; [parent log](../../.artifacts/native-tooling/native-generation.log).
- Linux arm64 release artifacts and checksums are verified metadata, **not native
  arm64 execution evidence**. Native arm64 setup, generator output and integration
  checks remain required. glibc Linux is the intended native baseline; other
  libc/OS environments are not validated here. Native execution also does not
  retain Docker's filesystem/network/resource sandbox.
- This tooling work does not establish the broader release, security, browser or
  compatibility gates. Real PostgreSQL lifecycle, process rehearsals and baseline
  tests are owned by the parent and fixture work.
