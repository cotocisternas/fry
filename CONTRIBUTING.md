# Contributing to FRY

FRY is an experimental Linux email gateway. Read the [README](README.md) for a
local start, [development tooling](docs/development.md) for prerequisites and
commands, and the [code map](docs/code-map.md) to find the module you will change.
Use the [glossary](GLOSSARY.md) and the relevant contract or validation record to
understand the behavior callers already rely on.

## Design for the next contributor

A module's interface includes its ordering rules, ownership, cancellation and
error behavior, as well as its types. Keep that interface small and let the
implementation own the complexity. A useful extraction gives callers one place
to obtain coherent behavior and maintainers one place to fix it.

Share mechanics when callers have the same responsibility and invariants.
Similar-looking permission checks, transaction stages or cleanup paths may serve
different policies. Read each path before combining it. Avoid pass-through
layers, generic repositories, callback frameworks or interfaces introduced only
to make a linter quiet. Where repetition preserves distinct responsibilities,
keep it and document the reason locally.

Use named constants or types for repeated domain states and concepts. Equal
strings alone do not establish a shared concept. Preserve exact protocol bytes,
SQL meaning and fixture expectations; independent tests should retain literal
expected values where sharing an implementation constant could hide a regression.

## Preserve ownership and durable outcomes

Keep current authorization, the mutation and its audit in the transaction that
owns them. Preserve organization isolation and reauthorization in established
sessions. Moving a check or adding an independently committing helper can change
the result even when the code looks equivalent.

Make context and resource ownership explicit. Pass the caller's context for
request work; a process or durable job with a different lifetime needs an explicit
owner and cancellation path. Preserve lock scope, transaction lifetime, defer
order, read-pin lifetime and cleanup after cancellation. Cleanup failure or an
uncertain commit/stream completion must retain its existing meaning.

Return errors with enough context at the place that understands the operation,
while preserving identities callers inspect with `errors.Is` or `errors.As`.
Keep transport error mapping in the transport adapter. Do not hide cleanup
failures or turn an uncertain result into success.

For storage changes, follow the [storage protocol](docs/design/storage-protocol.md).
A hotstore locator alone cannot authorize SMTP success: receiptstore owns the
two-copy-plus-metadata acceptance sequence. Preserve bounded streaming, checksums,
exact original bytes and the required completion checks.

## Test behavior through interfaces

Exercise the same module operations callers use and assert observable outcomes.
A refactor should retain independent assertions rather than replace them with
checks of helper calls or duplicated implementation logic. Use real PostgreSQL
for database locking, rollback and publication behavior; an in-memory adapter
cannot establish those contracts.

Add regressions for changed mechanics and newly exposed failure paths. Depending
on the change, that can mean denied authorization, rollback, interrupted or
corrupt streams, cancellation, uncertain retries, cleanup or shutdown. Assert
exact bytes and protocol replies where the contract requires them. Helpers that
report test failures should call `t.Helper()` at their start.

Coverage metrics are secondary evidence that can help locate gaps. Add tests for
behavior and failure paths, never extra assertions solely to reach a number.
Structural changes can legitimately affect the coverage denominator; explain the
accounting change and the behavioral evidence retained. Do not tune floors,
exclusions or package layout to conceal a regression. Keep the existing complexity
limits. A narrow linter exception must name the check and explain the particular
invariant or responsibility. Blanket test exclusions and broad suppressions obscure
the behavior under review.

## Choose checks for the change

Run commands from the checkout after [setup](docs/development.md#setup). The
[Makefile](Makefile), [quality adapter](scripts/quality.sh) and
[development guide](docs/development.md) define the command behavior. The
[script guide](scripts/README.md) covers launchers and scenario prerequisites.
Start with the affected packages and behavior; expand when shared contracts or
an observed failure warrant it.

For a handwritten Go change, select the package tests and lint scope. For example,
these commands check the message-index module using the pinned native tools:

```sh
./scripts/toolchain.sh test-go test -count=1 ./internal/messageindex
./scripts/toolchain.sh lint run --config .golangci.yml --allow-serial-runners ./internal/messageindex/...
```

`make fmt` applies the repository formatters to owned handwritten Go;
`make fmt-check` checks formatting. `make build` builds all four commands and
`make vet` vets the root and patched mail modules. Select these when the change
affects compilation, shared interfaces or the corresponding checks.

For database behavior, use the integration runner with package and test filters.
This migration example uses the same selection mechanism as the development guide:

```sh
FRY_TEST_RUN='Migration|Import' FRY_TEST_PACKAGES='./internal/receiptstore ./internal/imapmigrate' \
  ./scripts/tests/receipts.sh -race -short
```

The runner creates a disposable PostgreSQL fixture from the locally built images.
A skipped PostgreSQL test in `make test` is incomplete verification. Filtered runs
are partial evidence; record the packages, test selection and flags. `-short`
omits expensive large-message cases, so include the relevant large-message tests
when changing streaming or allocation behavior.

Choose additional checks by the files and contracts involved:

- SQL inputs: regenerate with `make sqlc`, then use `make sqlc-check sqlc-vet`
  and the owning module's PostgreSQL tests.
- Protobuf or browser generator inputs: use `make generate` and
  `make generate-check`. `make proto-drift-check` checks current schema and
  generated-output drift; it does not prove compatibility with a released version.
- Mail-library patches: use `make test-forks TEST_FLAGS=-race`. The nested modules
  are outside root `./...`; transport changes should include their relevant suites.
- Dependency-fixture changes: use `make test-deps-lint` and `make test-deps-test`,
  rebuilding images with `make test-deps-build` when recipes change. Use
  `make test-scripts` for Docker-free script regressions;
  `make test-script-processes` selects the process timeout and cleanup subset.
- Cross-module durable or concurrency changes: consider `make test-integration`
  or `make test-race`, which include the root PostgreSQL and both mail-fork suites.
  Use the relevant documented process rehearsal when startup, restart or wire
  behavior is part of the change.
- Documentation-only changes: verify local links and command claims. Go lint and
  runtime tests do not validate prose.

`make baseline-check` reproduces the existing-code baseline when that full
comparison is needed. It is not the complete future PR/release gate. Several
broader gates deliberately fail until implemented; see
[development tooling](docs/development.md#generation-and-tool-upgrades).

Daily work needs only the Make commands relevant to the change. Failure
rehearsals and benchmarks are opt-in checks with different assertions, such as
restart durability, restore correctness or retained-storage cost. Select them
when those contracts change; executing every script is not a daily requirement.

## Respect source ownership and explain the change

Keep shell scripts responsible for tool selection and short command
orchestration. Put configuration generation, report handling, protocol checks
and process lifecycle logic in normal Python modules under `scripts/`, invoked
with `python3 -m` through the repository toolchain. Keep JavaScript tooling logic
in ordinary JavaScript files. Do not use Python or JavaScript heredocs or
`-c`/`-e` program strings as permanent implementations. Heredocs containing only
text or configuration are fine.

Edit generator inputs and regenerate; never hand-edit generated `dbgen`, protobuf
clients or browser RPC bundles. The [code map](docs/code-map.md#source-and-generated-output)
identifies their owners. Keep upstream fork changes separate from stylistic
cleanup and run their own suites when changing them. Generated code still needs
compilation, reproducibility and runtime verification.

Describe the concrete problem, resulting behavior, contract preserved or changed,
and verification performed. Identify skipped prerequisites and partial evidence.
Update the relevant reference or validation document when behavior changes, and
keep design proposals distinct from implemented behavior. A local result does not
establish production readiness or an unexecuted architecture/recovery guarantee.
