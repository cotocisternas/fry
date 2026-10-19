# Engineering foundation

Implementation was authorized on 2026-10-18. This document records accepted
requirements, including work not yet implemented. Use the
[current implementation status](../implementation-status.md#current-engineering-status)
and [administration API gap list](../reference/admin-rpc.md#unavailable-foundations-and-compatibility)
to distinguish those requirements from current behavior.

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
  remains the command interface. FRY owns the Testcontainers-Go fixture code;
  service and cleanup-helper images come from digest-pinned upstream releases
  through an explicit pull. The earlier source-built-image interpretation was
  superseded by the user's clarification. FRY application binaries are built
  natively and copied into the pinned distroless runtime image.
  Ordinary builds, lint and generation do not use Docker. Docker remains for
  dependency services and explicit container/deployment rehearsals.

## Accepted implementation priorities

This is the agreed sequence, not a checklist of unfinished work. Native tooling,
the existing-code quality gate, typed Connect/member transports and Linux amd64
CI are implemented. Service accounts, several API mechanisms, complete operational
observability and release qualification remain pending; see the status links above.

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
7. Prepare maintainer-triggered releases from an exact selected commit:
   Linux binaries, multi-architecture images, checksums, SBOMs and signed build
   provenance. Expensive load and failure rehearsals are release gates.

CI checks the implemented engineering standards; it is not a substitute for
improving the code and tests. The repository is now public with a PR-only main
ruleset and required `CI`. Consequential refactors receive independent review.

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
tool artifacts and compatibility on upgrades. The existing-code cleanup and
blocking checks have been implemented; the policy continues to apply to changes.

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

The original private-source requirement was superseded by the user's explicit
public-visibility instruction. `cotocisternas/fry` is public and `main` requires
PRs and passing CI without bypass actors. GitHub Releases and GHCR remain the
proposed artifact stores; release visibility must be settled when that workflow
is implemented, since GitHub Releases in this repository would be public.
A maintainer selects a version and explicitly triggers publication after release
checks. The visibility change does not publish a release. The repository,
Actions workflow and branch rules are operational; release publication remains
unimplemented.

## Separate production gates

Metadata HA, trusted crash recovery/fencing, replica repair, degraded operation,
representative throughput/retention scale, actual AWS behavior and AZ-loss
validation remain separate work. No AWS validation access or execution has been
recorded. Passing
this engineering foundation does not establish the production durability,
availability, cutover or scale targets in the architecture brief.

## Historical initial toolchain baseline

Go 1.27.2 was verified as the latest stable release on 2026-10-18 using the
[official release history](https://go.dev/doc/devel/release). The local compiler
and the then-current Docker build used it; the root module minimum and `.go-version`
agreed. Current packaging builds binaries natively. Nested modules are tested
with the same compiler; the IMAP fork's language minimum was subsequently raised
during dependency remediation. The existing `go-imap/v2 v2.0.0-beta.8`
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

These were local baseline checks, not the later hosted CI result or proof of
production readiness. Temporary test database containers were removed. See the
[CI recovery record](../validation/ci-recovery.md) for the later tested revision.

## SQL and telemetry implementation

The user added sqlc and OpenTelemetry to this phase. The
[SQL and telemetry design](sql-and-telemetry.md) records generated-query ownership,
streaming exceptions, process configuration and privacy boundaries. sqlc 1.31.1
and OpenTelemetry Go 1.47.0 were verified against upstream releases. This work
adds real instrumentation and query generation. The gRPC contracts, broader lint
gate and amd64 CI were subsequently implemented. Release workflows and the
remaining observability signals are still separate work.

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

The module layout and command/test inventory are accepted. Implemented tool pins,
generated clients and coverage floors are recorded in the repository; proposed
numerical defaults for unfinished API mechanisms still require validation.
Per-method recovery contracts must be resolved before those mechanisms are
implemented. This accepted design does not replace the current API references.
