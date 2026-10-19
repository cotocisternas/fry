# Go lint gap audit

Status: pre-change research and policy recommendation, 2026-10-18, followed by
the implementation update below. This supplements [Go quality policy](go-quality-policy.md).

Subsequent implementation: the user prioritized contributor-facing code quality.
The current policy now includes `dupl` at 100 tokens, the explicit correctness
subset of `gocritic`, `makezero`, `nosprintfhostport`, begin-only `thelper`,
`errname` and `predeclared`. The counts and catalog decisions below remain the
pre-change audit record. Other candidates have not been enabled wholesale.

The later [modernization pass](../validation/go-modernization.md) also enabled
`modernize`, `copyloopvar`, `unconvert`, `asasalint`, `bidichk`, `durationcheck`
and `gocheckcompilerdirectives`. Treat the disabled-linter catalog below as a
historical snapshot; `.golangci.yml` and the current quality policy define the
active checks.

The previous green baseline established compliance with the configured policy.
It did not establish that every useful analyzer was enabled, that duplication was
absent, or that the program was defect-free. There are useful gaps. Keep an
explicit policy, select subchecks deliberately, and remediate or document findings
before making additional checks blocking.

## Evidence and scope

Read `.golangci.yml`, the existing policy, the native catalog from
`./scripts/toolchain.sh lint linters`, and tagged upstream sources. Native
`lint version` identifies **2.15.0**, built with **go1.27.0**, commit `25c5369a`.
The catalog lists **34 enabled and 81 disabled linters**, including three
deprecated names. Formatter entries are separate. Upstream's
[tagged dependencies](https://github.com/golangci/golangci-lint/blob/v2.15.0/go.mod),
[settings reference](https://github.com/golangci/golangci-lint/blob/v2.15.0/.golangci.reference.yml)
and implementation take precedence over assumptions based on a linter's name or
an unversioned documentation example.

The parent task ran isolated candidate audits; this research read their JSON
artifacts without rerunning lint. `.artifacts/lint-gap/root.json` contains
**764 reports from 23 candidate linters**; `testdeps.json` contains one additional
report. These runs measure candidates, rather than the accepted configuration.
Counts are diagnostics, not independent defects. They describe the selected
build and modules, not every platform, generated file, or upstream fork.

| Candidate | Root reports | Interpretation before adoption |
| --- | ---: | --- |
| `dupl` | 5 at 150 tokens; 11 at 100 | All production; overlapping and reciprocal matches. See calibration below. |
| `goconst` | 448: 97 production, 351 test | Literal occurrences, not 448 distinct constants or defects. |
| `gocritic` | 12: 8 production, 4 test | Stable defaults include style checks; select correctness checks explicitly. Tooling adds one. |
| `thelper` | 29 test | 21 parameter naming reports, eight helper-start reports. |
| `protogetter` | 195: 128 production, 67 test | Review presence and validation semantics before conversion. |
| `containedctx`, `predeclared`, `recvcheck` | 13, 10, 3 | Context ownership, builtin shadowing, mixed pointer/value receivers respectively. |
| `usestdlibvars`, `nestif`, `canonicalheader` | 23, 8, 6 | Standard constants, nesting, header spelling; different policy value. |
| `mirror`, `errname`, `iface`, `unqueryvet` | 3, 2, 2, 2 | Mirror reports are tests; identical interfaces may serve distinct consumers. |
| `makezero`, `errchkjson`, `tparallel` | 1 each | Each has an intentional or already documented interpretation below. |
| `exptostd`, `loggercheck`, `nakedret`, `nosprintfhostport`, `promlinter` | 0 | Zero reports do not establish relevance or additional coverage. |

Separate `revive-defaults.json` contains **391 Revive reports**: 238 `exported`,
109 `context-as-argument`, 32 `unused-parameter`, ten `redefines-builtin-id`, one
`package-comments`, one `indent-error-flow`. The source policy remains unchanged.

## Recommended first five groups

1. **Code and literal duplication:** adopt `dupl` at an explicit **100-token**
   threshold after reviewing the 11 reports; use `goconst` to identify repeated
   domain literals, with a tuned policy rather than mass constant extraction.
2. **Focused correctness:** explicitly select `gocritic` checks, and add
   `makezero` with `always: false` and `nosprintfhostport` after reviewing their
   exceptions. Avoid importing the full default style suite accidentally.
3. **Test diagnostics:** enable `thelper` helper-start checks for test,
   benchmark, fuzz and TB helpers; disable its naming and parameter-position
   checks initially. Review `tparallel` findings without requiring every test
   group to run in parallel.
4. **Small naming safeguards:** add `errname` and `predeclared` after review.
   Do not enable Revive's overlapping builtin rule as well. Treat `recvcheck`
   as a separate pointer/value receiver decision.
5. **Boundary and lifetime review:** triage `protogetter`, `containedctx`,
   `unqueryvet` and optional vet checks. These deserve attention, but their raw
   totals do not justify automatic rewrites or immediate blanket blocking.

These were the recommendations at the time of the audit. The implementation
update above identifies the adopted checks. Keep narrow, explained exceptions;
preserve meaningful independent tests,
authorization distinctions, transaction ownership and cancellation behavior.

## Duplication calibration

`dupl` compares serialized AST structure and ignores node values: differently
named variables and different literal values can match. Its threshold counts
structural tokens, not lines. The bundled adapter analyzes the files supplied
to each analysis pass; it is not a semantic, whole-repository clone proof.
See the [bundled fork's algorithm](https://github.com/golangci/dupl/blob/c99c5cf5c202/README.md),
[adapter](https://github.com/golangci/golangci-lint/blob/v2.15.0/pkg/golinters/dupl/dupl.go)
and [150-token integration default](https://github.com/golangci/golangci-lint/blob/v2.15.0/pkg/config/linters_settings.go).
The standalone README's 100-token default is distinct from golangci-lint's default.

At 150, all five reports concern the similar `Domains`, `Aliases` and
`MailAccounts` operations in `internal/admin/directory.go:121-215`. Some spans
overlap adjacent functions. At 100, `dupl100.json` adds reciprocal matches for:

- `internal/admintransport/legacy.go:394-407` and `409-422`
  (`submissionPassword` / `submissionAccess`).
- `internal/receiptstore/hot_cleanup_linux.go:501-519` and
  `sealed_reclaim_linux.go:459-477` (bounded pipe copy and checksum verification).
- `internal/receiptstore/rotation_linux.go:19-44` and
  `sealed_reclaim_linux.go:24-49` (dispatch).

**Recommend 100 explicitly:** eleven production reports are a manageable review
set, and the stream-copy match is useful. Group reciprocal/overlapping reports
before deciding what to extract. Share mechanics only when the resulting helper
preserves bounds, checksum handling, lock scope, cleanup and transaction ownership.
Similar authorization skeletons may intentionally enforce different policies.
Keep tests in scope, with local reasons for intentional repetition; do not erase
independent behavioral coverage to satisfy a clone heuristic.

`goconst` detects equal complete literal values; it cannot determine whether they
represent the same concept. Prefer named domain states, recurring business error
messages and existing standard constants. Repeated protocol bytes, SQL fragments,
addresses and expected fixture output may be clearer literally; tests should not
derive every expected value from the implementation's constant. Start calibration
with `min-len: 3`, `min-occurrences: 3`, `numbers: false`, `match-constant: true`,
and tests visible. Review function-call/map-key settings explicitly; prefer narrow
exceptions over blanket test exclusions. The **97 production / 351 test** split
argues for semantic triage. See [goconst matching behavior](https://github.com/jgautheron/goconst/blob/v1.11.0/README.md)
and the [integration settings](https://golangci-lint.run/docs/linters/configuration/#goconst).

## Selected checks and important caveats

**Gocritic:** use `disable-all: true` and explicit `enabled-checks`, initially
`appendAssign`, `badCall`, `badCond`, `caseOrder`, `dupArg`, `dupBranchBody`,
`dupCase`, `dupSubExpr`, `exitAfterDefer`, `offBy1`. These target suspicious
assignments, calls, conditions and copied expressions. Defer stylistic defaults
such as `ifElseChain` and `sloppyLen`; do not select all tags or experimental
checks automatically. This is a proposed selection, not a separately executed
audit. See [tagged check descriptions](https://github.com/go-critic/go-critic/blob/v0.15.0/docs/overview.md)
and [integration](https://github.com/golangci/golangci-lint/blob/v2.15.0/pkg/golinters/gocritic/gocritic.go).

The observed default-check reports are four `exitAfterDefer` across root and
tooling, three `appendAssign`, three `badCall`, two `ifElseChain`, one `sloppyLen`.
`os.Exit` skips deferred cancellation, but these reports alone demonstrate no
resource leak: inspect what the separate execution function already owns and
cleans up. Single-argument `filepath.Join` reports also require inspection.
The authorization test's append intentionally extends an already-read prefix;
both `appendAssign` and `makezero` flag it. It is not evidence of accidentally
prepending empty elements. Keep `makezero.always` false, preserving legitimate
indexed initialization. See [makezero's scope](https://github.com/ashanbrown/makezero/blob/v2.2.1/README.md).

**Test checks:** `thelper` distinguishes `begin`, `name` and `first` settings
for `test`, `benchmark`, `tb` and `fuzz`. Select `begin: true`, `name: false`,
`first: false` for each; the useful initial cleanup is the eight helper-start
reports, not renaming 21 TB parameters. See [thelper subchecks](https://github.com/kulti/thelper/blob/v0.7.1/README.md).
`tparallel` checks cleanup and parent/subtest parallel consistency. Its one report
is a sequential parent grouping parallel subtests; that arrangement is valid Go
and may deliberately bound concurrency. Inspect shared state and cleanup before
changing it. See [tparallel's rules](https://github.com/moricho/tparallel/blob/v0.3.2/README.md)
and [Go's grouping semantics](https://pkg.go.dev/testing#hdr-Subtests_and_Sub_benchmarks).

**Boundary checks:** `protogetter` is applicable to the handwritten RPC code,
not just generated code. Getters safely return defaults for absent receivers,
which can also mask a distinction between missing and explicit zero values.
Retain required-field validation and intentional presence checks; no bulk fix
of the 195 accesses. See [protogetter](https://github.com/ghostiam/protogetter/blob/v1.0.1/README.md)
and [protobuf accessor semantics](https://protobuf.dev/reference/go/go-generated/).
`containedctx` finds stored contexts but cannot establish incorrect ownership.
Classify its 13 reports as request, stream/job lifetime or test adapter first;
retain justified lifetime-bound objects. Go recommends per-call contexts and
documents exceptions in [Contexts and structs](https://go.dev/blog/context-and-structs).

`recvcheck` detects mixed **pointer/value receiver types**, whereas the existing
Revive `receiver-naming` rule governs receiver variable names. Review its three
types for copying, mutation and interface method sets; avoid blanket pointer
conversion. See [recvcheck](https://github.com/raeperd/recvcheck/blob/v0.3.1/README.md).
`errname` concerns `Err...` sentinels / `...Error` types; `predeclared` concerns
builtin shadowing. Neither is supplied by receiver naming.

`unqueryvet` reports two wildcard queries: schema/migration concatenation and a
test `pgx.Exec`. Inspect their purpose before changing query shape. The default
wildcard checks do not imply its optional N+1, SQL-injection and transaction-leak
checks are enabled: `check-n1`, `check-sql-injection`, `check-tx-leaks` default
false in the pinned integration. Review those independently; generated Go
exclusion also means linting does not validate sqlc input SQL. See
[unqueryvet](https://github.com/MirrexOne/unqueryvet/blob/v1.5.4/README.md) and
[pinned settings](https://github.com/golangci/golangci-lint/blob/v2.15.0/.golangci.reference.yml).

`errchkjson` adds type analysis but intersects the strict `errcheck` policy.
Its one report is `json.Marshal` of a fixed string/bool shape, already carrying
an explained `errcheck` exception. An `errcheck` suppression does not suppress
`errchkjson`. Inspect types and tune `check-error-free-encoding` deliberately;
do not copy the upstream suggestion to exempt every Marshal call. See
[errchkjson's safe/unsafe types](https://github.com/breml/errchkjson/blob/v0.4.1/README.md).

`nosprintfhostport` covers naive formatted URL authorities and IPv6 brackets;
the existing vet `hostport` pass overlaps address construction but should not be
assumed to cover every URL-format pattern. See [the analyzer's scope](https://github.com/stbenjam/no-sprintf-host-port/blob/v0.3.1/README.md).

## Complete disabled-catalog decision matrix

All 81 disabled names from the native catalog appear below. Decisions are FRY
recommendations based on the evidence above and the upstream
[catalog/settings](https://golangci-lint.run/docs/linters/configuration/), not
assertions that each rule found a defect.

| Group | Disabled linters | Decision |
| --- | --- | --- |
| Useful; add after remediation | `dupl` | Explicit 100-token threshold, clone-group review and local justified exceptions. |
| Useful; add after remediation | `gocritic`, `makezero`, `nosprintfhostport` | Select correctness subchecks; preserve intentional appends and cleanup ownership. |
| Useful; add after remediation | `thelper` | Begin-only helper diagnostics. |
| Useful; add after remediation | `errname`, `predeclared` | Small naming safeguards; avoid duplicate builtin checks. |
| Tunable / review first | `goconst` | Repeated domain literals, rather than mechanical fixture deduplication. |
| Tunable / review first | `canonicalheader`, `mirror`, `usestdlibvars` | Inspect direct header-map indexing and standard equivalents; distinguish convention/performance from bugs. |
| Tunable / review first | `containedctx`, `recvcheck`, `tparallel` | Lifetime, method-set and scheduling decisions require context. |
| Tunable / review first | `protogetter`, `unqueryvet`, `errchkjson` | Boundary correctness and semantic exceptions described above. |
| Tunable / review first | `iface`, `iotamixing`, `testableexamples`, `gochecksumtype` | Review duplicate interface contracts, enum conventions, executable examples and deliberately sealed sum types; no forced interface centralization. Sum-type coverage differs from existing enum exhaustiveness. |
| Tunable / review first | `modernize`, `exptostd`, `usetesting` | Review individual modernizations; existing `intrange` already owns integer-range policy. |
| Tunable / review first | `gomoddirectives`, `gomodguard_v2`, `forbidigo`, `reassign` | Useful once prohibited modules, directives, APIs or mutable globals are named; preserve local mail-module replacements. |
| Tunable / review first | `musttag`, `tagliatelle`, `importas` | Only with explicit serialization and alias conventions; avoid unintended wire changes. |
| Deliberately defer / overlap | `gocyclo`, `nestif`, `maintidx`, `interfacebloat` | Existing cyclomatic/cognitive/length limits suffice; eight nesting reports do not justify another complexity gate. |
| Deliberately defer / overlap | `forcetypeassert`, `wastedassign`, `goprintffuncname` | Partial overlap with strict errcheck, assignment analysis and vet; require a concrete uncovered pattern. |
| Deliberately defer / overlap | `loggercheck` | Handwritten code uses slog; vet's `slog` plus `sloglint` already cover relevant call shape/style. Revisit wrappers or other logger APIs. |
| Deliberately defer / overlap | `err113`, `wrapcheck` | Broad error-identity/wrapping policies need API agreement; existing `errorlint` addresses wrapping correctness. |
| Deliberately defer | `paralleltest`, `testpackage`, `ireturn`, `gochecknoglobals`, `gochecknoinits`, `exhaustruct_v5` | Do not mandate concurrency, external test packages, one return abstraction, or universal initialization/global bans. |
| Deliberately defer | `prealloc`, `perfsprint` | Profile relevant paths before adding performance-driven churn. |
| Deliberately defer | `asciicheck`, `decorder`, `dogsled`, `dupword`, `embeddedstructfieldcheck`, `funcorder`, `godoclint`, `godot`, `godox`, `goheader`, `gosmopolitan`, `grouper`, `inamedparam`, `misspell`, `mnd`, `nakedret`, `nlreturn`, `noinlineerr`, `nonamedreturns`, `tagalign`, `varnamelen`, `whitespace`, `wsl_v5` | Additional style/documentation conventions have lower priority. `dupword` already matched intentional protocol fixtures; existing formatters own layout. |
| Dependency irrelevant today | `arangolint`, `clickhouselint`, `ginkgolinter`, `testifylint`, `zerologlint` | No corresponding handwritten API usage found in root/tooling; an indirect dependency alone is insufficient. Revisit when usage appears. |
| Dependency irrelevant today | `promlinter` | FRY instruments OpenTelemetry, not Prometheus client declarations. This linter does not validate OTel metric names. |
| Deprecated; defer | `exhaustruct`, `gomodguard`, `wsl` | Do not adopt deprecated names; maintained successors are classified above. |

The logger overlap conclusion follows [loggercheck's recognized APIs](https://github.com/timonwong/loggercheck/blob/v0.13.0/README.md).
The metric scope follows [promlinter's documentation](https://github.com/yeya24/promlinter/blob/v0.3.0/README.md).

## Already-enabled suites still have policy gaps

**Vet:** enabling `govet` does not enable every pass. Compare `allAnalyzers` with
`defaultAnalyzers` in the [pinned adapter](https://github.com/golangci/golangci-lint/blob/v2.15.0/pkg/golinters/govet/govet.go),
rather than inferring coverage from the linter list. Review optional
`deepequalerrors`, `nilness`, `reflectvaluecompare`, `sortslice`, `unusedwrite`
for incremental value against Staticcheck. Keep `shadow` as a separate noisy
audit, `fieldalignment` for measured memory work, `findcall` for named API rules,
`atomicalign` for applicable architectures, and `httpmux` for applicable Go
compatibility targets. `slog`, `hostport`, `lostcancel`, `copylocks` and
`waitgroup` are already in this version's default list; do not claim them as gaps.
Some checks overlap, and absence of a report is not proof of absence of a bug.

**Revive:** only `receiver-naming` is configured; its default rules are explicitly
disabled. The 391-report experiment does not justify enabling defaults wholesale.
`context-as-argument` accounts for 109 reports entirely in test helpers using
`(t, ctx)`; its documented `allow-types-before` can accommodate testing types.
If adopted, retain context-first production APIs and configure that narrow helper
allowance rather than reorganizing all test signatures. Choose either
`predeclared` or `redefines-builtin-id` for the ten overlapping shadow reports.
Inspect the 32 unused parameters against required interface/callback signatures
and existing `unparam`; document comments and error-flow layout are separate
policy choices. These rule meanings and tuning are documented in the
[tagged Revive rules](https://github.com/mgechev/revive/blob/v1.17.1/RULES_DESCRIPTIONS.md).

Other enabled suites are also selected policies: `spancheck` currently runs
`end` only, `depguard` restricts specific transport/CLI persistence imports, and
`sloglint` has an explicit context setting. None implies all tracing, architecture
or logging rules are covered. Maintain the explicit list, review subcheck changes
on upgrades, and keep behavioral, race, integration and security evidence separate
from a clean lint result.
