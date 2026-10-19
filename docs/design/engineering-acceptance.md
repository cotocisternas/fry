# Engineering foundation acceptance design

Implementation was authorized on 2026-10-18. This document records accepted
requirements, including work not yet implemented. Use the
[current implementation status](../implementation-status.md#current-engineering-status)
and [administration API gap list](../reference/admin-rpc.md#unavailable-foundations-and-compatibility)
to distinguish those requirements from current behavior.

Status: accepted as part of the consolidated design, 2026-10-18, including the
command layout, test inventory and sequencing. Numerical limits remain subject
to validation. Implementation is authorized; artifact publication remains subject
to the agreed maintainer-triggered release workflow.

The Linux amd64 contributor gate and container packaging have now
[passed hosted CI](../validation/ci-recovery.md). This establishes the implemented
checks, not every proposed behavior case below: retry/revision/jobs, service
accounts and resumable downloads remain pending. Native arm64, release artifacts
and production recovery/scale qualification remain separate gates.

## Accepted quality gates

All existing FRY-owned handwritten code must meet the selected lint/format policy,
including tests, with narrow documented exceptions. Use the accepted
[explicit linter roster and formatters](../research/go-quality-policy.md).
Generated code remains subject to compilation, reproducibility, vulnerability
analysis and real integration tests. Upstream fork code gets a separate baseline;
FRY-maintained changes do not receive a blanket exemption.

Critical behavior needs explicit regression cases as well as statement-coverage
measurement. Establish reviewed per-package floors from the initial baseline and
ratchet them upward. Exclude generated code from the handwritten percentage while
also retaining an unfiltered report and an explicit accounting rule. Moving code
between packages must not disguise a regression. A high percentage cannot replace
an authorization-denial, crash-boundary or interrupted-transfer case.

Every PR requires native Linux amd64 core race tests, real PostgreSQL integration
tests and both patched mail-library suites. Native arm64 runs of those suites are
release gates. Browser and architecture-independent checks need not be duplicated.
Required gates fail if prerequisites are missing rather than passing with skipped
PostgreSQL/browser/contract tests. Ordinary lightweight local commands can remain
available, but must be labeled as incomplete verification.

PR gates also include lint/build, protobuf generation and compatibility checks,
core browser flows, vulnerability/secret/static-security checks. High/critical
security findings block unless covered by a reviewed, time-limited exception.
Where a source lacks severity labels, findings require explicit triage rather
than being treated as low severity automatically.

## Proposed behavior-case inventory

Test through the same application interfaces used by protocol adapters. Unit tests
exercise deterministic rules; real PostgreSQL tests establish transaction and
locking behavior. Avoid mock query strings as proof of database correctness.

- Administrative authorization: organization isolation, explicit action grants,
  content-access separation, OIDC mapping and revoked permissions on subsequent
  operations, including retries and resumed downloads.
- Mutations: concurrent same-ID submissions, changed contents under one ID,
  successful commit with lost response, rollback, expired retries, stale resource
  revisions and one-time credential-secret disclosure.
- Long operations: durable submission, reconnect, safe cancellation, a completion
  racing with cancellation, partial outcomes and operation-specific recovery.
- Downloads: byte identity across ranges, invalid ranges and validators, resumed
  access after revocation or expiry, bounded read protection and interrupted writes.
- Member RPC: bounded chunks and total bytes, truncation/corruption, cancellation,
  final status, owner/generation checks and uncertain durable outcomes. Preserve
  existing receipt acceptance invariants during transport replacement.
- Existing SMTP/IMAP/storage: preserve supported protocol behavior, exact original
  bytes, mailbox state, cleanup rules and bounded query plans while refactoring.
- UI/CLI: successful administration and denied/stale/uncertain results, reconnecting
  to operations and useful errors that do not disclose credentials or mail content.
- Telemetry: privacy and bounded labels, context propagation, collector outage,
  disabled behavior and bounded shutdown. Observability failures must not change
  a completed mail/storage outcome.

These cases cover functionality within the foundation scope. They do not add a
promise that the unfinished HA, fencing, replica repair or production-scale work
has been completed. Applicable existing failure rehearsals remain meaningful;
future production guarantees require their own implementation and evidence.

## Proposed developer command contract

The reference workflow uses mise-managed native tools and Make, with exact tool
versions and locked dependencies. Docker runs digest-pinned upstream test dependencies with FRY-owned fixtures
and explicit deployment rehearsals. Local development and CI use the same versions and
semantics. Proposed targets: `make fmt`, `make lint`, `make generate`,
`make generate-check`, `make test`, `make test-integration`, `make test-race`,
`make test-browser`, `make security`, `make check`, `make build` and
`make release-check`. Final names remain reviewable design details.

Formatting writes only when explicitly requested; check targets report drift.
Generation checks regenerate in an isolated temporary tree and compare artifacts.
`make check` denotes the complete PR-equivalent gate, not merely unit tests.
Release checks add native architecture evidence and the applicable expensive
load/failure rehearsals. Local invocations must not publish artifacts.

Record test environments and prerequisites. Use isolated synthetic databases and
storage, explicit time/resource budgets, deterministic failure controls and useful
failure diagnostics. Do not hide flaky failures behind unconditional retries.
Keep discovered fuzz failures as regression inputs; sustained fuzzing cadence and
budgets remain a proposed part of the detailed test plan, not an active schedule.

## Proposed sequencing and release evidence

1. Inventory the existing behavior/tests, measure coverage, record owned fork
   changes and establish the reviewed baseline without claiming untested behavior.
2. Introduce the repeatable tool workflow, apply lint/format policy and add missing
   critical regressions. Complete the existing-code baseline as a phase gate.
3. Refactor module responsibilities in reviewable steps while preserving their
   transaction, authorization, streaming and resource-ownership contracts.
4. Introduce the accepted Connect/admin/internal contracts, service-account flows
   and transfer behavior, with relevant wire and integration cases.
5. Enforce the agreed checks in GitHub CI and prepare release workflows.
   Bind native amd64/arm64 results, generated-code checks and artifact provenance
   to the exact release revision and recorded dependency/tool inputs.

The maintainer selects a version and explicitly triggers publication. Produce the
agreed Linux binaries and multi-architecture images, checksums, SBOMs and
signed provenance. The source repository is now public; artifact visibility must
be explicit when the release workflow is implemented. Passing foundation gates
does not establish production HA or scale. Real AWS validation still requires
account access and remains a separate
gate; local synthetic fixtures must not be described as AWS validation.
