# Go quality policy

Status: researched 2026-10-18. The user accepted a clean lint/format baseline
across all existing FRY-owned handwritten code, with narrow documented exceptions.
The user also accepted required critical-behavior regressions plus a per-package
coverage ratchet after initial baseline review. The user subsequently accepted
the explicit lint/format policy below, including blocking findings with narrow
documented exceptions. Exact tool artifacts and any repository-specific exceptions
still require verification during adoption; the unresolved govulncheck pin is not
implicitly accepted as latest stable.
Implementation update, 2026-10-18: the user replaced the container toolchain with
mise-managed native tools and locally built Testcontainers dependency images.
See [development tooling](../development.md) for the current workflow.
The follow-up [lint gap audit](lint-gap-audit.md) records duplication findings and
explicit recommendations for every disabled linter. The contributor-quality pass
adopts its duplication, selected correctness, helper-entry and naming checks.
Its remaining candidates still require individual review.
The existing correctness baseline passed before RPC adoption; the stricter
complexity baseline below now passes across existing code. The handwritten
baseline covers the four FRY commands and shared packages. Both patched nested
mail modules retain explicit runtime/security checks; upstream code and FRY's
maintained changes need distinct lint policies. Typed Connect-Go administration and
internal gRPC transports are implemented; the stricter integrated baseline is
tracked in [the follow-up validation record](../validation/sql-rpc-complexity.md).

## Verified version facts and proposed pins

- Go **1.27.2**, released 2026-10-08, is the current stable release in the
  [official release history](https://go.dev/doc/devel/release#go1.27.2).
- golangci-lint **v2.15.0**, released 2026-10-16, is marked latest by
  [upstream](https://github.com/golangci/golangci-lint/releases/tag/v2.15.0).
  Go 1.27 support arrived in v2.13.0 ([changelog](https://github.com/golangci/golangci-lint/blob/main/CHANGELOG.md)).
  Its module minimum is Go 1.26.0, which does **not** identify the compiler used
  for a distributed binary. Verify the downloaded binary's build compiler: lint
  supports targets no newer than that compiler, and new Go support also requires
  analyzer changes ([compatibility policy](https://golangci-lint.run/docs/welcome/faq/)).
- Prefer its bundled gofumpt **v0.12.0**, goimports from x/tools **v0.52.0**,
  Staticcheck **v0.8.2**, and gosec **v2.29.0**; these are verified bundle
  versions, not claims that every standalone tool is latest
  ([tagged dependencies](https://raw.githubusercontent.com/golangci/golangci-lint/v2.15.0/go.mod)).
- govulncheck **v1.9.0**, published 2026-10-11, is an available stable candidate
  ([package record](https://pkg.go.dev/golang.org/x/vuln@v1.9.0/cmd/govulncheck));
  its [tagged module](https://raw.githubusercontent.com/golang/vuln/v1.9.0/go.mod)
  requires Go 1.26.0. The unversioned package page inconsistently warns about
  latest status, so latest stable is **not conclusively verified** here.
  Reconfirm that pin and actual Go 1.27 analysis before adoption.

## Accepted blocking lint and formatting checks

Start with `linters.default: none` and an explicit list. Avoid enable-all: new
releases would silently expand policy and add overlapping stylistic demands.
Use normal analyzer defaults within the listed linters, then review changes on
tool upgrades. The first five below are upstream's
[default roster](https://golangci-lint.run/docs/welcome/quick-start/).

| Linters | FRY purpose |
| --- | --- |
| `govet`, `staticcheck`, `errcheck`, `ineffassign`, `unused` | Suspicious constructs, API misuse, ignored errors and dead work. |
| `errorlint` | Wrapping, comparison and assertion mistakes with errors. |
| `contextcheck`, `noctx` | Lost inherited contexts and contextless network calls. |
| `bodyclose`, `sqlclosecheck`, `rowserrcheck` | HTTP body/SQL resource ownership and iteration errors. |
| `spancheck` (`end` only) | Unfinished telemetry spans. |
| `gosec` | Security-sensitive source patterns. |
| `nolintlint` | Require a specific linter and substantive suppression explanation. |
| `cyclop`, `gocognit`, `funlen` | Maximum cyclomatic complexity 10; cognitive reporting threshold 15; maximum 60 lines and 40 statements. |
| `nilerr`, `nilnesserr`, `nilnil`, `unparam` | Suspicious nil/error results and unused parameters. |
| `exhaustive`, `fatcontext`, `sloglint` | Exhaustive enum handling, context lifetime, and contextual structured logs. |
| `depguard` | Administration transports and CLI cannot import generated persistence packages directly. |
| `revive` (`receiver-naming` only) | Consistent, idiomatic receiver names across methods of a type. Default Revive rules are disabled explicitly. |
| `lll`, `intrange` | Maximum 120 columns, counting tabs as four; integer-range loops when behavior is preserved. |
| `copyloopvar`, `unconvert` | Redundant loop-variable copies and unnecessary conversions. |
| `asasalint`, `durationcheck`, `bidichk`, `gocheckcompilerdirectives` | Variadic slice mistakes, suspicious duration arithmetic, bidirectional text controls and malformed compiler directives. |
| `dupl` | Review structural duplication at 100 tokens; shared logic must preserve semantic responsibilities. |
| `gocritic` (explicit selection), `makezero`, `nosprintfhostport` | Suspicious calls, assignments, conditions, copied expressions, skipped defers, slice initialization and host/port construction. |
| `thelper` (`begin` only) | Keep failures attributed to callers of test, benchmark, fuzz and TB helpers. |
| `errname`, `predeclared` | Consistent error identities and avoidance of shadowed built-in names. |

Quality for contributors means understandable responsibilities, localized domain
knowledge, explicit resource ownership and tests of useful behavior. Metrics and
linters support those goals. Do not preserve unnecessary code to protect a
coverage denominator, introduce shallow helpers to hit a function-length target,
or merge different policies merely because their syntax matches. Prefer direct
code and a narrow explained exception when an abstraction would make maintenance
harder. See [contributor guidance](../../CONTRIBUTING.md).

Gocritic explicitly enables `appendAssign`, `badCall`, `badCond`, `caseOrder`,
`dupArg`, `dupBranchBody`, `dupCase`, `dupSubExpr`, `exitAfterDefer` and `offBy1`.
Its default style suite is disabled. `makezero.always` is false. Thelper's naming
and parameter-position rules are disabled; helper-entry checks remain enabled.
`goconst` stays a semantic review tool, with local constants for genuinely shared
domain values rather than automatic replacement of fixtures and wire literals.

Style-policy update, 2026-10-18: the previous green baseline did not enforce
receiver consistency, line length or integer-range modernization. Those checks
now join the explicit policy. `golines` wraps handwritten Go at 120 columns with
four-column tabs through `make fmt`; `lll` also catches comments and literals
that the formatter cannot wrap. Keep generated code and upstream fork code under
their existing separate policies. Do not add blanket test or string exclusions.
Split long string constants without changing their bytes; preserve justified
statement-level exceptions when formatting moves a call across several lines.

`make lint-policy-check` tests the actual configuration with intentional naming,
line-length and integer-range violations in production and test files, verifies
strict generated-code exclusion, then verifies the corrected fixture passes.
It is part of the existing-code baseline.
The cleanup and passing integrated checks are recorded in the
[style-policy validation](../validation/lint-style.md).

The broader audit evaluated `modernize` and `dupword` as well. The former reported
196 root-module suggestions, including 75 `newexpr`, 73 `rangeint` and ten
`waitgroupgo` findings. Integer ranges are enforced by `intrange`; the remaining
modernization suite is not enabled wholesale. Pointer construction, iterator and
goroutine changes require their own semantic review. `dupword` also matched
intentional repetitions in protocol/message fixtures, so it is not a blocking
rule. The six additional focused checks above had no initial root findings.
The checker and formatter behavior is documented by
[golangci-lint](https://golangci-lint.run/docs/linters/configuration/),
[Revive](https://github.com/revive-lint/revive/blob/master/RULES_DESCRIPTIONS.md#receiver-naming)
and the [golines settings](https://golangci-lint.run/docs/formatters/configuration/#golines).

Complexity limits apply to tests as well as production code. Extract coherent
validation, transaction stages, fixtures and behavioral assertions; preserve
transaction ownership, error propagation and all test assertions. A suppression
must name one function or statement and explain why the control flow belongs
together. Do not copy broad test exclusions from other projects. Scrap supplied
the 10/15 complexity reference; function-length limits are FRY policy.

These checks are documented in the [linter catalog](https://golangci-lint.run/docs/linters/).
Configure errcheck with `check-blank`, `check-type-assertions` and
`disable-default-exclusions` enabled. Explicitly discarded results still need a
local rationale. SQL checks have API recognition limits; verify pgx coverage and
wrapper recognition during adoption. Review context creation at process/job
boundaries; inherited cancellation is not always correct for detached work
([settings](https://golangci-lint.run/docs/linters/configuration/)).

Use bundled `goimports`, `gofumpt` and `golines` for handwritten files; keep optional extra
gofumpt rules disabled initially. Formatting is a separate check (`fmt --diff`),
since `run` is linting ([formatter docs](https://golangci-lint.run/docs/formatters/configuration/),
[CLI](https://golangci-lint.run/docs/configuration/cli/)).

## Visibility, generated output and security

Keep exclusion presets empty. v2 defaults already use an empty preset list;
copying sample presets can hide unchecked Close/Flush errors, permission issues,
subprocesses and variable file paths ([configuration](https://golangci-lint.run/docs/configuration/file/),
[preset contents](https://golangci-lint.run/docs/linters/false-positives/)).
Enable unused-exclusion warnings. Avoid blanket test-file exclusions or omitting
nested modules from validation. Apply the clean lint/format baseline to FRY-owned
code; upstream portions of the forks need a documented separate baseline, not a
mass stylistic rewrite. Test each full fork and analyze its security exposure.
Use line-scoped exceptions with reason; security exceptions also record an owner
and expiry.

Recognize generated files by the strict standard marker for lint/format output
exclusion. This must not omit sqlc/protobuf/Connect code from compilation,
vulnerability analysis, reproducibility checks or runtime integration tests.
Review generator inputs and test database/query behavior through public storage
operations; avoid manually fixing generated output. Handwritten SQL streaming
exceptions retain the full lint and test policy.

`go vet` finds likely correctness bugs; [its heuristics are incomplete](https://pkg.go.dev/cmd/vet).
[gosec](https://github.com/securego/gosec) checks security patterns in source.
[govulncheck](https://go.dev/doc/security/vuln/) matches known vulnerable symbols
and call paths for the selected build. Run all three roles; none establishes
security by itself. Keep the compiler's vet check for generated code as well.
Record vulnerability database freshness. Go advisories have no universal severity
labels: triage exposure explicitly instead of relying on a high/critical filter.

## Coverage and runtime evidence

Measure package/function **statement** coverage before setting a floor; Go's
[coverage instrumentation](https://go.dev/blog/cover) does not measure branches
or independently cover both sides of short-circuit expressions. Publish a
handwritten-code metric alongside an unfiltered report; keep the denominator
explicit. Ratchet agreed package baselines; do not silently lower established
floors. Changes to accounting or package structure need review so they cannot
disguise a coverage regression.
Do not invent an 80/90% target or infer failure-path confidence from a percentage.

Require named behavior cases for authorization denial, cancellation/deadlines,
rollback and PostgreSQL failure, interrupted streams, invalid/oversize input,
resource cleanup and graceful shutdown. After Connect implementation, add wire
error mapping and stream cancellation. Run real-PostgreSQL integration/race tests
and both nested module suites explicitly; root `./...` is insufficient.
Build all four commands for Linux amd64/arm64, and label execution evidence per
architecture. [Race detection](https://go.dev/doc/articles/race_detector) supports
both Linux targets but detects only executed races; cross-compilation supplies no
runtime evidence. The local baseline is implemented and passing; native arm64
execution and the remaining security/CI/release gates still require implementation.

## Native CI architecture feasibility

GitHub supports standard Linux arm64 hosted runners for private repositories,
including `ubuntu-24.04-arm`. Usage counts toward the plan's included minutes;
actual account availability, limits and budget still need validation. This makes
native amd64/arm64 checks a feasible design option without requiring FRY's AWS
account. The user selected native amd64 core race/PostgreSQL/fork checks on every
PR and native arm64 runs of those suites before releases. Cross-compilation does
not satisfy the arm64 release gate. Browser and architecture-independent checks
need not be duplicated. No workflow has been created.
[GitHub announcement, 2026-01-29](https://github.blog/changelog/2026-01-29-arm64-standard-runners-are-now-available-in-private-repositories/).
