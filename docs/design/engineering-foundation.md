# Engineering foundation

Implementation authorization update, 2026-10-18: the user explicitly instructed
“start the work.” The earlier design-only pause is superseded. Execute the
accepted sequence, beginning with code quality and meaningful test baselines.
Delegated work uses medium or high effort and standard execution only; no low,
fast/priority, max or ultra modes.

Status: consolidated design accepted and implementation authorized, 2026-10-18.
Improve existing code and tests first, preserving atomic authorization, audit
and mutations, then introduce the accepted APIs and enforce quality gates.
Numerical limits remain provisional until validated during implementation.
Artifact publication still requires the agreed maintainer-triggered release.

## Agreed boundaries

- Refactor the prototype freely. Breaking API/configuration/schema changes and
  database resets are allowed; no existing installation needs migration.
- Preserve the FRY server, storage member, migration tool and administration CLI
  process boundaries. Refactor their internal modules and transports.
- Support Linux amd64 and arm64. Distinguish cross-compilation from tests actually
  executed on each architecture.
- Verify current stable releases before selecting Go, build tools and dependencies.
  Pin exact versions and container digests; update related pins together. Record
  prerelease dependencies as explicit exceptions for review.
- Tooling revision accepted 2026-10-18: mise manages pinned native tools and Make
  remains the command interface. Testcontainers-Go owns disposable dependencies;
  their service and cleanup-helper images are built locally from repository
  recipes. Pinned Linux bases and verified build dependencies may be downloaded.
  Ordinary builds, lint and generation do not use Docker. Docker remains for
  dependency services and explicit container/deployment rehearsals.

## Planned implementation priority (after design approval)

1. Measure package coverage and identify untested behavior. Establish strict
   formatting/lint/static checks and focused regression tests before structural
   changes. Suppressions must be narrow and justified.
2. Improve module responsibilities, interfaces, dependency direction, errors,
   contexts, timeouts, resource cleanup, configuration validation and API
   documentation. Coverage numbers locate gaps; critical failure paths require
   explicit tests. Avoid tests that merely mirror implementation details.
3. Introduce versioned protobuf contracts and gRPC for administration and internal
   service communication. The selected framework is Connect-Go, with Connect-Web
   for the administration UI and gRPC over mutual TLS internally. RPC-style
   HTTP/JSON is sufficient; a REST gateway is not required. All transports use
   the same application authorization. See [ADR 0023](../adr/0023-use-connect-for-protobuf-rpc.md)
   and the [API contract design](api-contract.md). Preserve SMTP/IMAP interoperability
   and storage streaming, durability and ownership guarantees. Internal peers
   require matching release versions initially; document coordinated upgrades.
4. Preserve local/OIDC human authentication. Add dedicated service accounts with
   rotatable FRY-managed client credentials exchanged for short-lived tokens.
   Permissions name actions and organization/global scope. Content access stays
   separate; human roles map to predefined permission sets. Recheck current
   authorization on every operation; already-authorized work may finish.
5. Standardize structured logs, Prometheus-compatible metrics, optional
   OpenTelemetry tracing and health/readiness contracts. Exclude message bodies,
   attachment contents and credentials. Keep metric cardinality bounded.
6. Enforce the quality baseline in GitHub Actions on hosted Linux runners. Every
   PR requires builds/lint, real-PostgreSQL Go race tests, both patched mail-library
   suites, protobuf compatibility/generated-code checks and core browser tests.
   Missing required prerequisites fail the gate. Add vulnerability scanning,
   secret detection and static security checks; high/critical findings block
   merges unless a reviewed exception records a rationale, owner and expiry.
7. Prepare private, maintainer-triggered releases from an exact selected commit:
   Linux binaries, multi-architecture images, checksums, SBOMs and signed build
   provenance. Expensive load and failure rehearsals are release gates.

CI enforces these engineering standards throughout; it is not a substitute for
improving the code and tests. Consequential refactors receive independent review.

## Accepted code-quality baseline

The foundation phase must bring all existing FRY-owned handwritten code into
compliance with the selected formatting and lint rules before the phase is
complete, rather than enforcing them only on new or changed lines. Exceptions
must be narrow and documented. Generated code and upstream dependency code need
separate policies; excluding generated code from stylistic lint must not remove
its compilation, reproducibility or behavior checks.

The user accepted a test-quality gate combining required regression cases for
critical behavior with a per-package coverage ratchet. Measure and review the
initial baseline before establishing floors; thereafter the package floors may
increase but must not be silently lowered to accommodate a change. Generated code
is excluded from the handwritten coverage percentage while still exercised by
real integration tests. A high percentage alone does not satisfy the gate.

Critical behavior includes authorization/isolation, durable outcomes and uncertain
retries, transaction rollback, interrupted or corrupt streams, cancellation,
resource cleanup and shutdown for the functionality in scope. Keep separate the
existing later HA/scale gates. The concrete case inventory and coverage accounting
must be reviewed during the planned baseline work.

The user accepted the [Go quality policy](../research/go-quality-policy.md):
golangci-lint with an explicit correctness, error-handling, context,
resource-cleanup and security roster, plus gofumpt and goimports. Findings block
the baseline unless a narrow exception is documented. Keep generic exclusion
presets empty; repository-specific exceptions need explicit review. Verify exact
tool artifacts and compatibility during adoption. These are requirements for
future implementation, not permission to begin cleanup during design.

## Accepted native-test cadence

Every PR requires the core Go race tests, real-PostgreSQL integration tests and
both patched email-library suites on native Linux amd64. Native Linux arm64 runs
of those suites are required before a release, rather than on every PR. This
cadence trades earlier arm64 feedback for lower routine CI usage. Cross-compiling
arm64 artifacts is not a substitute for the native release gate. Browser and
architecture-independent checks need not be duplicated across architectures.

This refines the previously agreed PR/release checks and keeps both architectures
in the official support scope. The full release matrix and evidence binding to
the exact release revision remain part of the detailed design.

## Repository and publication

The agreed destination is `cotocisternas/fry`, private, with private artifacts
and container images. GitHub Releases and GHCR are the proposed artifact stores.
A maintainer selects a version and explicitly triggers publication after release
checks. Scope agreement does not itself publish a release. Repository setup and
remote enforcement remain implementation work, subject to actual account access
and available GitHub features.

## Separate production gates

Metadata HA, trusted crash recovery/fencing, replica repair, degraded operation,
representative throughput/retention scale, actual AWS behavior and AZ-loss
validation remain separate work. AWS account/access is not yet available. Passing
this engineering foundation does not establish the production durability,
availability, cutover or scale targets in the architecture brief.

## Initial toolchain baseline

Go 1.27.2 was verified as the latest stable release on 2026-10-18 using the
[official release history](https://go.dev/doc/devel/release). The local compiler
and Docker build already used it; the root module minimum, `.go-version` and
README now agree. Nested dependency modules retain upstream language minimums
and are tested with the same compiler. The existing `go-imap/v2 v2.0.0-beta.8`
dependency remains a prerelease requiring explicit stability review; this
baseline does not claim every dependency has been updated or is stable.

The first PostgreSQL-backed race run failed on two pre-existing issues: the quota
UI test captured a neighboring form handler, and the member-storage file seek
could choose an unbounded filtering plan. Both were repaired, with focused
regressions and independent review. Final validation with `GOTOOLCHAIN=local`:

- `./scripts/tests/receipts.sh -race -short` passed against PostgreSQL 18.6;
  receiptstore 101.744s, administration 80.457s, IMAP 69.539s. The short suite
  excludes the separate large-message stress cases. Log:
  `/tmp/fry-go1272-final-race.log`.
- Both nested mail-library race suites passed with `-count=1`. Logs:
  `/tmp/fry-go1272-imap-race.log` and `/tmp/fry-go1272-smtp-race.log`.
- `go vet ./...` passed. Log: `/tmp/fry-go1272-vet.log`.

These are local baseline checks, not the future CI/release gate or proof of
production readiness. Temporary test database containers were removed.

## SQL and telemetry implementation

The user added sqlc and OpenTelemetry to this phase. The
[SQL and telemetry design](sql-and-telemetry.md) records generated-query ownership,
streaming exceptions, process configuration and privacy boundaries. sqlc 1.31.1
and OpenTelemetry Go 1.47.0 were verified against upstream releases. This work
adds real instrumentation and query generation; the gRPC contracts, broad lint
baseline, CI/release workflows and remaining observability signals are still
separate implementation work.

[Local validation](../validation/sql-and-telemetry.md) passed for generated-code
reproducibility, the complete short PostgreSQL race suite, both patched mail
libraries, real-process OTLP/mail smoke, vet and the Linux arm64 command build.
The arm64 result is cross-compilation, not native execution.

## Current design frontier

The accepted policies now include Connect-Go/Connect-Web, resumable HTTPS original
downloads, bounded internal gRPC transfers, retry/revision/job behavior, six months
of API-major overlap, the explicit lint roster, a clean existing-code baseline,
critical regression tests and coverage ratchets. Native amd64 suites run on PRs;
native arm64 suites are required before releases.

The accepted consolidated design consists of:

- [Module boundaries](engineering-modules.md): responsibilities, dependency
  direction, private sqlc types, explicit telemetry and transaction composition.
- [API contract](api-contract.md): accepted client-visible policy.
- [API mechanisms](api-mechanisms.md): request-ID expiry recognition,
  job ownership, safe download resumption, bounded transfers, errors and cursors.
- [Engineering acceptance](engineering-acceptance.md): critical test
  inventory, Make command contract, implementation sequence and release evidence.
- [Go quality policy](../research/go-quality-policy.md): accepted lint/format
  roster and coverage accounting requirements.

The module layout and command/test inventory are accepted; numerical protocol
defaults remain provisional until validated. Exact compatible tool artifacts, generated-client
packaging and per-method permission/recovery contracts must be resolved before
their corresponding implementation steps. Coverage floors require measurement;
they are not invented during design. Design acceptance is separate from an
explicit subsequent instruction to begin implementation.
