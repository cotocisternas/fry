# Receiver naming, line length and integer ranges

Scope: the 2026-10-18 follow-up to the engineering baseline. The user reported
receiver-name differences, long lines and counted loops that could use integer
range. This change strengthens the handwritten-code policy and cleans its
existing findings.

## Policy

- Revive's `receiver-naming` rule requires consistent receiver names.
- `lll` enforces 120 columns with four-column tabs. `golines` joins gofumpt and
  goimports in the formatter; comments and literals still need deliberate edits.
- `intrange` enforces eligible integer-range loops. Conversion must preserve
  counter types, bound evaluation and loop behavior.
- The explicit policy also enables `copyloopvar`, `unconvert`, `asasalint`,
  `bidichk`, `durationcheck` and `gocheckcompilerdirectives`.

Production code and tests use the same rules. Strictly marked generated files
keep their existing exclusions, while generation, compilation and integration
checks still exercise them. Upstream mail-library code retains its separate
policy and complete fork test suites.

Existing complexity limits, coverage floors and test assertions remain in force.
Long constants are split without changing their bytes. Where wrapping makes a
function too long, coherent setup or validation stages are extracted while
preserving transaction, cleanup and error behavior. Existing narrow suppressions
retain their rationale and are attached to the statements they describe.

## Verification

`make lint-policy-check` runs the actual repository configuration against a
temporary fixture. It requires receiver-name, line-length and integer-range
findings in both production and test files, checks generated-code exclusion, and
then requires the corrected fixture to pass. The check is included in
`make baseline-check`.

The first integrated run passed the root race tests with PostgreSQL 18.6, but
correctly failed the unchanged CLI coverage floor: removing repeated request
factories changed the denominator, yielding 289/356 statements (81.2%) against
82.3%. Added request-validation regressions prove malformed paths, unknown
operations, invalid/overflowing page limits and unknown protobuf fields are
rejected before sending any network request. Focused CLI race tests now cover
294/356 statements (82.6%). No floor was lowered.

Two independent static reviews found no material issues across the policy,
commands, services and storage changes. They checked helper extraction,
transaction/lock/defer lifetimes, error ordering, integer-range bounds and
preserved assertions against the original source snapshot. Exact `io.EOF`
identity checks remain intact. Decoded-string audits found no changed query or
message bytes; the only removed literal occurrences came from consolidating
duplicated search-criteria construction and removing an unused protobuf import.
The search helper still creates fresh criteria for each call.

Final verification: **`make baseline-check` passed** on 2026-10-18 with native
Go 1.27.2 on Linux amd64 and the locally source-built PostgreSQL 18.6 fixture.
The run passed:

- Configuration validation, the lint-policy regression, strict lint, formatting,
  vet and builds.
- SQL generation/vet and protobuf Go/TypeScript/browser bundle reproducibility.
- Dependency-tooling lint and race tests, plus process-cleanup regressions.
- Root `-race -short` tests with real PostgreSQL and every unchanged per-package
  coverage floor.
- Both complete patched email-library race suites.
- Identical source-input hashes before and after the run.

Local evidence: `.artifacts/lint-style/final-baseline.log`,
`.artifacts/baseline-check/{PASSED,root.json,root.out}` and its before/after source
manifests. The first coverage failure is retained in
`.artifacts/lint-style/first-baseline.log`.

The broad `modernize` suite remains an audit tool rather than a blocking rule:
its pointer-construction, iterator and goroutine suggestions need separate
semantic review. This cleanup does not complete the outstanding release,
security, CI, native arm64 or production-scale gates.

See [the policy and research](../research/go-quality-policy.md) and
[development commands](../development.md).
