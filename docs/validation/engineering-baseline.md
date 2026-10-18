# Engineering foundation baseline

Existing-code baseline passed, 2026-10-18. This records the existing-code quality phase before the
accepted module and RPC changes. It is not production-readiness or AWS evidence.
The later central-SQL, RPC and complexity work has a separate
[validation record](sql-rpc-complexity.md); results below describe this earlier baseline.
The full engineering gates are specified in
[the acceptance design](../design/engineering-acceptance.md).

## Tooling and scope

The [reference workflow](../development.md) pins Go 1.27.2, Node 26.12.0 and
golangci-lint 2.15.0 in digest-pinned containers. The explicit linter roster applies
to handwritten FRY commands, packages and tests. Generated code retains build,
generation, security and integration obligations. The two patched mail modules
run separate test suites; root-module tests do not include them.

The initial strict scan reported 1,794 findings. Cleanup addresses error handling,
contexts, resource ownership, security-sensitive operations and formatting.
Exceptions are local and explained; security exceptions include owner and expiry.
Suppressions must not convert a wrapped protocol/integrity error into successful
EOF or turn an already durable acceptance into a new retry on cleanup failure.

## Critical behavior added or strengthened

- An HTTP destination write failure records an interrupted content read even
  when the source reader already consumed the entire original. The regression
  failed before the `Content.WriteTo` correction.
- Reading exactly the advertised byte count without observing verified EOF
  records an interrupted read. Independent review identified this gap; the
  public content/audit regression failed before explicit EOF tracking was added.
- Revoking content access denies a fresh content operation while an already
  authorized read can finish. Cross-organization content requests fail without
  opening bytes or recording an authorized content-access audit.
- Failure to append an administrative audit rolls back inbox creation and alias
  replacement. A subsequent successful operation publishes the intended change.
- Wrapped source EOF remains an error in message indexing and byte storage.
  Stream integrity failures must not be mistaken for normal completion.
- A durable hot-store append or verified retry preserves its locator if later
  staging cleanup fails; subsequent writes are fenced until reconciliation.
- Object range-close failures remain visible rather than silently succeeding.
- IMAP source cleanup verifies the full retained original before flagging or
  expunging the source. A real protocol/PostgreSQL regression exposed a prior
  zero-length range request that skipped payload verification. Injected payload
  read and close failures now preserve the source UID, bytes and flags; a later
  successful retry removes it while keeping one destination receipt. The close
  failure reaches the draining read, so this is not evidence of an independent
  read-handle-close-only failure path.
- Member and packer construction accept a caller context for bounded startup.
  Cancellation before initialization leaves no registration; cancellation after
  successful initialization does not terminate the object's process lifetime.
  Acceptance, full reads and published-pack reads exercise the surviving objects.
- Member restart gives metadata stages separate deadlines before and after the
  local byte-store scan. A deterministic regression expires the prior stage and
  verifies a restarted member still reads accepted originals. Cancellation during
  scanning releases ownership when the synchronous scan returns; it does not
  interrupt the filesystem scan itself.
- Failed-maintenance outcome recording has a five-second detached database
  budget. Four real PostgreSQL lock-contention cases verify eventual return,
  preservation of the original failure, frozen-member behavior and readable
  accepted content.
- A close failure on an over-limit rejected connection records a sanitized
  diagnostic and permits subsequent member listener admission.

These cases use real PostgreSQL/application operations, actual protocol adapters
or named storage failure controls. They supplement statement coverage rather than
using a percentage as evidence of failure-path correctness.

## Verification recorded so far

Linux amd64, pinned container workflow, PostgreSQL 18.6:

- Scoped race suites passed for administration, SMTP, IMAP, source IMAP,
  hot/object storage, migration, outbound, database schema, directory,
  replica configuration and telemetry. Database-backed runs used disposable
  PostgreSQL. These are partial runs, not the final combined gate.
- The combined Go/Node container ran both Node administration tests, a real
  PostgreSQL telemetry case and both short patched-library suites successfully.
- Both complete patched-library suites subsequently passed with `-race` in the
  reference container, without `-short`.
- Combined root-module lint and formatter checks passed after the constructor
  context regressions were added.
- A deliberately unformatted scratch source made the formatter check fail;
  the check reports drift without rewriting sources.
- All four commands compile for Linux amd64 and arm64. The arm64 result is
  cross-compilation evidence only; it does not replace native execution.
- sqlc generation reproduced the existing generated files without drift, and
  sqlc query vet passed. The checkout currently has no Git metadata.
- Coverage accounting rejects invalid/stale source ranges and accepts Go's
  legitimate zero-statement empty-function records. Its focused tests and lint
  pass. Numerical floors were reviewed against the complete run described below.

The second combined root run passed all packages with real PostgreSQL, race
detection and `-short`. This excludes the explicitly separate large-message
rehearsals. Its raw profile is retained at `.artifacts/foundation/root.out`;
reporting failed because the profile also contains records from the two patched
dependency modules. Explicit module accounting now keeps those records visible
separately without entering FRY's handwritten coverage floors. The reporter's
race tests and a fresh focused profile through `make coverage-report` pass;
artifacts are under `.artifacts/coverage-accounting-smoke`. That smoke is not a
replacement for a fresh complete baseline after source changes.
The fork records in this root run do not replace the separate fork test suites.

The subsequent complete root run, including the storage review fixes and module
accounting, passed with real PostgreSQL, `-race -short -coverpkg=./...`, and a
five-minute package timeout. Evidence is in `.artifacts/foundation-baseline`:
`coverage.log`, `root.out`, `root.json`, `source.sha256` and `source-check.log`.
All recorded source hashes matched after the run. Combined lint/format, vet of
root and both forks, and all command builds also passed there. The root report
contains 29 packages; dependency execution is reported separately in seven IMAP
packages and one SMTP package. The earlier complete fork race evidence remains
applicable because those sources are unchanged.

Independent review verified all 466 source hashes and reproduced coverage counts
from the raw profile. The reviewed floors in `tooling/coverage-floors.json` round
handwritten percentages down to one decimal place. Generated-only packages retain
an explicit zero floor and an undefined handwritten percentage. Measured gaps include
`cmd/fry-member` (0/102 handwritten statements), `cmd/fry-migrate` (0/130),
`installation` (0/3), `cmd/fry` (132/673), `replicaconfig` (55/159), and the load
tool command (31/316). These are explicit coverage gaps, not generated-code
exclusions. Application/protocol integration tests do not establish that these
command configuration and lifecycle paths have executed.

The first combined
coverage attempt failed in the SMTP handshake/shutdown test's cleanup assertion;
that partial profile is not an accepted baseline. Diagnosis reproduced the
unsynchronized failure 3/10 times, then 3/3 after synchronizing handshake startup.
The corrected test explicitly expects the deadline and checks server/client
connection cleanup; 10/10 focused race/coverage runs passed, with related
transport cases passing five repetitions. Deadline errors are not globally
ignored. Full vet also exposed unkeyed
upstream IMAP header literals, now fixed in a separate documented compatibility
patch. The combined vet command now passes for the root and both fork modules.
The latest source snapshot and successful coverage evidence are in
`.artifacts/foundation-baseline`.

An independent storage review identified an overall startup timeout that can be
exhausted by a large local recovery scan, unbounded detached maintenance cleanup,
and a rejected-connection close error that can stop the member listener. Fixes
and regressions passed the complete receiptstore race suite with `-short`, plus
scoped lint and formatting. The latest combined run includes those fixes. An
independent follow-up review found all three resolved and no material introduced
regressions by inspection; it did not rerun tests. The review
also noted two intentional availability tradeoffs: uncertain maintenance freezes
the member until recovery, and read-pin cleanup failure remains a terminal read
error even if payload bytes have already streamed. Neither is relaxed into a
successful storage outcome.

Fresh initialization that creates bytes but fails before registration leaves
unregistered file evidence and refuses silent adoption on retry. This existing
policy is covered by `TestMemberFilesFreshCatalogInsertFailureRollsBackBinding`;
the new cancellation-during-scan case covers reopening an already registered
member. Automatic recovery of an unregistered fresh directory is not claimed.

Browser, security and protobuf gates explicitly fail as incomplete until their
implementations exist. Native arm64 execution and real AWS validation have not
been established by these Linux amd64 local checks.

## Enforced baseline and behavior inventory

`make baseline-check` enforces the reviewed floors, complete root core race tests
with PostgreSQL and Node (`-short`), complete patched-library race suites without
`-short`, lint, formatting, vet, command builds and sqlc reproduction/query checks.
It refuses test selection and floor/output overrides, and compares source
snapshots before and after execution. Only a successful unchanged-source run
writes `.artifacts/baseline-check/PASSED`. This existing-code gate does not replace
the broader unfinished `make check` or release gate. `make coverage` and
`make coverage-report` also use the reviewed floors by default.

The first end-to-end invocation passed all core tests but failed the new floors:
hot storage measured 551/650 rather than 552/650, and SMTP 409/474 rather than
412/474. All recorded input hashes still matched. Comparing raw block records
isolated cancellation after staging and invalid DATA state; those branches had
only incidental execution in the earlier run. A new hot-store regression proves
cancellation after staging publishes nothing, releases staging/admission capacity,
and permits a subsequent append whose bytes survive reopening. It passed ten
focused race executions and failed when the cancellation guard was removed; the
production source was restored exactly. SMTP now has deterministic wire and
public backend-contract cases for new/reset/missing-recipient/duplicate DATA.
They verify no unintended acceptance, connection/session cleanup, and correct
subsequent transactions. Ten focused PostgreSQL race repetitions passed; a
success-return mutation failed all four backend cases. That mutation check ran
without coverage because the pinned coverage build ignored the source overlay.
No production mutation was retained. The subsequent `make baseline-check` passed
with both deterministic regressions and the original reviewed floors unchanged.
The complete root core race suite, complete fork race suites, all static/build/sqlc
checks and before/after source comparison passed. Current evidence is in
`.artifacts/baseline-check`, with the full log at
`.artifacts/baseline-contract/second-gate.log`.
No passing gate marker was written for the failed first invocation.
Failed-run evidence is retained in `.artifacts/baseline-contract/first-run.*` and
`full-gate.log`. Test-selection and alternate-floor overrides were rejected;
an intentionally unmet floor also produced a nonzero result with a violation.

The named cases below run in the root stage of `make baseline-check`. For focused
diagnosis, use `FRY_TEST_PACKAGES=./internal/PACKAGE` and `FRY_TEST_RUN=TEST_NAME`
with `make test-integration TEST_FLAGS='-race -short'`; that is partial evidence.

| Package | Required existing behavior cases |
| --- | --- |
| `admin` | `TestHTTPInterruptedDownloadIsNotAuditedAsComplete`, `TestContentAuditRequiresVerifiedEOF`, `TestContentOnlyRevocationPreservesAuthorizedTransferAndMetadata`, `TestCrossOrganizationContentDenialReturnsNoHandleOrDownloadAudit`, `TestInboxCreationRollsBackWhenMutationAuditFails`, `TestAliasUpdateRollsBackListingAndRoutingWhenMutationAuditFails` |
| `imapcollect` | `TestCleanupPreservesSourceUntilRetainedOriginalVerified` |
| `hotstore` | `TestAppendCancellationAfterStagingReleasesAdmission`, `TestStagingCleanupPreservesDurableAppendAndRetry`, `TestWrappedEOFDoesNotPublishStaging`, `TestReadImageReportsRangeCloseFailures` |
| `messageindex` | `TestWrappedSourceEOFIsNotSuccessfulCompletion` |
| `smtpserver` | `TestSMTPBackendRejectsInvalidDataWithoutAcceptance`, `TestSMTPWireRejectsInvalidDataAndPreservesNextTransaction` |
| `receiptstore` | `TestMemberInitializationContextOwnership`, `TestPackerInitializationContextOwnership`, `TestMemberRestartScanDoesNotConsumeMetadataStageBudget`, `TestMemberInitializationCancellationDuringScanReleasesOwnership`, `TestFailedMaintenanceOutcomeWaitIsBoundedAndRemainsFenced`, `TestMemberListenerContinuesAfterRejectedConnectionCloseFailure`, `TestMemberFilesFreshCatalogInsertFailureRollsBackBinding` |

Floors are a regression floor, not an assertion that each package has sufficient
tests. Do not lower them automatically. New packages, package moves and changed
accounting require explicit review; preserve the cases above when moving code.
Review and raise floors after meaningful added behavior coverage.

Priority refactor prerequisites remain: real installation/schema-refusal tests
before storage creation; member startup failure and SIGTERM cleanup; migration
stale activation and output failure after committed actions; replica trust and
directory failures and releasing the first member if the second fails to open.
The load tool needs a tiny SMTP-to-IMAP round trip, cancellation and integrity
mismatch case. Add these before changing the corresponding module responsibilities.
Future API retry/revision/cancellation/resume, service-account, browser, security,
native arm64, large-message and HA/scale cases remain later explicit gates.

## Refactor follow-up evidence

After the accepted baseline snapshot, eight new installation/replica-configuration
cases passed scoped PostgreSQL 18.6 race tests, lint and formatting. They cover
installation composition, incompatible schemas, missing catalog before byte-file
creation, invalid directories/trust/fingerprints (including live TLS pin mismatch),
and closing the first member when opening the second fails while preserving
accepted originals. A cleanup-removal mutation failed the last regression as
intended; no production change was needed. These additions await the next combined
run and an upward coverage review. Command-level startup/shutdown and migration
cases are still being implemented separately.

The RPC toolchain also passed `make proto-tools-check` on Linux amd64: offline
Go/TypeScript generation, Go compilation, TypeScript checking, browser bundling,
deterministic regeneration, drift refusal without rewriting, stale cleanup and
generated-directory ownership checks. Log: `.artifacts/rpc/make-tools-check.log`.
This synthetic fixture does not establish actual FRY RPC interoperability or
schema compatibility. Native arm64 execution remains unverified.
