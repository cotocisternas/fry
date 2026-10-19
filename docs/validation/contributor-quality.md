# Contributor quality pass

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Validated locally on 2026-10-18, Linux amd64, native Go 1.27.2 and the
repository-built PostgreSQL 18.6 fixture. This pass improves maintainability and
behavioral evidence before introducing more contributors. It does not establish
production readiness or complete the engineering foundation.

The fixture image and scope are historical; [upstream test images](upstream-test-images.md)
and the [hosted CI result](ci-recovery.md) record the later integrated path.

## Changes and preserved contracts

- Current-file and sealed-file reclamation share one bounded record-copy
  implementation. It closes the pipe reader on append failure, joins the source
  reader before returning, preserves both errors, and verifies size and SHA-256
  before the caller can publish a locator. Callers still own storage gates,
  source lifetimes, capacity policy and publication.
- Explicit maintenance shares local/remote dispatch and identity handling.
  Operation-specific recovery remains in each operation; automatic rotation
  retains its separate nonblocking admission path.
- Directory listings share validation, current authorization, policy-lock and
  transaction lifetime. Their typed queries and projections remain explicit.
  Legacy credential and access handlers retain distinct request schemas and
  audited operations. Local duplication exceptions explain these distinctions.
- Telemetry copies retained attribute storage so callers can reuse their slices
  without changing deferred metrics or having their backing arrays overwritten.
- The administration CLI uses an independent flag set per invocation and plans
  commands before authentication. Help succeeds, classified usage errors exit
  two, and execution failures exit one. Execution failures can still represent
  uncertain mutation outcomes. Signal cancellation runs before process exit in
  the CLI and affected test tools.
- The lint policy adds duplication detection at 100 structural tokens, selected
  correctness checks, allocation and host/port checks, test-helper diagnostics,
  error naming and builtin-shadowing checks. Repeated domain states use local
  constants; unrelated equal strings are not merged automatically. See the
  [policy](../research/go-quality-policy.md) and [audit](../research/lint-gap-audit.md).
- The [README](../../README.md), [contribution guide](../../CONTRIBUTING.md) and
  [code map](../code-map.md) give newcomers a shorter entry path. Historical
  validation links remain in [implementation status](../implementation-status.md).

## Verification

Passed:

```sh
make lint-config fmt-check lint vet build generate-check sqlc-vet
make lint-policy-check
make test-race TEST_FLAGS=-short FRY_TEST_TIMEOUT=5m
FRY_TEST_RUN='^TestRemoteLargeMessageUsesBoundedStreaming$' \
  FRY_TEST_PACKAGES='./internal/receiptstore' FRY_TEST_TIMEOUT=5m \
  ./scripts/tests/receipts.sh -race
./scripts/test-deps.sh test
```

The main race invocation used real PostgreSQL for the root packages and ran both
patched mail-library suites. The separate streaming invocation exercised the
200 MiB remote-message case omitted by `-short`. The fixture module also passed
its scoped lint and formatting checks. No coverage-floor adjustment was used to
accept this work.

New regressions exercise corrupt, canceled and failed copies; maintenance refusal
and gate release; listing isolation, revoked grants, empty-result shape, query
failure and canceled transaction cleanup; distinct legacy request schemas;
retained telemetry attributes; and CLI parsing without global state or premature
authentication. Existing exact-byte, storage-maintenance, authorization and
protocol tests remained in the integration run.

Two independent static reviews covered storage/CLI and administration/protocol/
telemetry changes against the pre-change snapshot. One inaccurate comment
claiming HSTS behavior was corrected; no material correctness findings remained.
The documentation pass validated local links and retained every original README
destination. Local execution logs are in `.artifacts/quality-sharing/`.

This is amd64 local evidence. It does not validate native arm64, AWS failure
domains, sustained production throughput, or the unfinished security, browser,
compatibility and release gates described in the
[development guide](../development.md#generation-and-tool-upgrades).
