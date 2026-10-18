# Coverage accounting

Run the standard-library-only reporter from the root Go module:

```sh
go run ./internal/devtools/coverage \
  -repo . \
  -profile /tmp/fry-foundation-coverage.out > /tmp/fry-foundation-coverage.json
```

The profile must already exist and come from the intended complete test run,
including real PostgreSQL tests. The reporter does not run tests or establish
that integration prerequisites were met. The initial reviewed root manifest is
`tooling/coverage-floors.json`; its evidence and limitations are recorded in the
[engineering baseline](engineering-baseline.md). Standard coverage targets apply
it by default; direct reporter invocation without `-floors` is accounting only.

Options:

- `-profile FILE`: required Go coverprofile, with `set`, `count` or `atomic` mode.
- `-print-coverpkg`: instead of reporting, print the `go test -coverpkg` value
  for the root and mapped modules (see below). It accepts `-repo` and `-module`
  only and validates mappings the same way.
- `-repo DIR`: measured module root containing `go.mod`; defaults to `.`.
- `-floors FILE`: optional reviewed JSON floor manifest for root packages; enables enforcement.
- `-module IMPORT=DIR`: repeatable explicit local dependency module mapping. `DIR`
  must be a canonical repository-relative directory containing a `go.mod` that
  declares exactly `IMPORT`; absolute paths, traversal, symlinks and duplicate
  module mappings fail. Unknown/unmapped module paths always fail.

The output is one JSON object with `mode`, the root import path in `module`, and
a sorted root-only `packages` array. Each
package has its import path in `package` and three objects, `handwritten`,
`generated`, and `unfiltered`. Each object contains integer `covered` and `total`
statement counts. `unfiltered` is exactly the sum of the two categories. A
statement block contributes its statement count to `covered` when its execution
count is positive, otherwise only to `total`. Duplicate blocks with the same
file/range and statement count contribute once, covered if any occurrence was
executed. Conflicting statement counts for duplicate ranges fail.

Generated classification uses Go's `ast.IsGenerated`: the strict standard
`// Code generated … DO NOT EDIT.` line must occur before the package clause.
Names, directories and approximate markers do not confer an exclusion. Source
files are parsed, and every profile entry is validated, including generated ones.
A category with zero total statements has no defined percentage; consumers
should display it as unavailable rather than as 100%.

Profiles must use canonical module-import paths such as `github.com/cotocisternas/fry/internal/storage/x.go`.
Files must exist under the supplied module root. Traversal, absolute paths,
backslashes and symlinked source paths fail. A source under a nested module
cannot be attributed to the root module. Explicitly map dependency modules as
described below, or report an independent module profile against its own module
root. The repository root
itself may be reached through a symlink. Source positions must be in range.
Invalid headers, records, source files, integer overflow, inconsistent duplicates,
empty measurement and missing final newlines fail. No partial report is emitted
for invalid input. A newline is required because Go writes one after every record;
it helps detect a truncated final record. Coverprofiles carry no completeness
checksum: truncation exactly between complete records, or omission of complete
records within a still-measured package, cannot be detected from the format alone.
Bind the profile to the exact unchanged source revision used by the complete test
command, including any working-tree edits; retain the command, successful test
exit status, revision and profile together. Do not edit sources between profile
generation and reporting. A stale profile may fail source-range validation, but
edits that leave ranges valid are not detectable from the coverprofile. Rerun the
full root coverage suite after source edits in any measured module, including
formatting, before reviewing
or enforcing the baseline. The root integration runner owns that full-coverage
rerun; this reporter never repairs or relaxes ranges to accept a stale profile.

## Profiles containing local dependency modules

Root test profiles contain instrumented dependency packages from the local IMAP
and SMTP replacements. `make coverage` takes its `-coverpkg` value from
`-print-coverpkg` with the same `-repo` and `-module` arguments as the report, so
it instruments exactly the accounted modules by import path:
`github.com/cotocisternas/fry/...,github.com/emersion/go-imap/v2/...,github.com/emersion/go-smtp/...`.
Do not use `-coverpkg=./...`. Go matches a relative pattern against the directory
of every loaded package, so it also instruments downloaded modules whenever
GOMODCACHE is inside the checkout, as in CI (`.artifacts/cache/go-mod`). Their
records belong to no mapping and the reporter rejects them. They also enter the
statement totals behind `go test`'s per-package `coverage:` summary lines, so
those percentages then differ from runs with the cache outside the checkout.

Keep the raw profile unchanged. Account for every record with explicit mappings
rather than removing foreign records or adding them to the root denominator:

```sh
go run ./internal/devtools/coverage \
  -repo . \
  -profile .artifacts/foundation/root.out \
  -module github.com/emersion/go-imap/v2=third_party/go-imap \
  -module github.com/emersion/go-smtp=third_party/go-smtp \
  > /tmp/fry-foundation-coverage.json
```

These flags declare the local module source locations intentionally; the reporter
checks each mapped `go.mod` and does not download modules or infer a mapping from
a filename. The longest matching module-import prefix selects the scope. All
mapped source paths, generated markers, positions and counts undergo the same
validation as root records, relative to their own opened `os.Root`. Further nested
modules require their own explicit mappings. At least one root package must be
measured.

When mappings are supplied, the additive `dependencies` JSON array contains one
object per mapping, sorted by module import path. Each object has `module`, the
repository-relative `directory`, and its own sorted `packages` array using the
same handwritten/generated/unfiltered count schema. An unused mapping has an
explicit empty `packages` array. Without mappings, `dependencies` is omitted.
The top-level `packages` always contains only the root module. Summing each
scope's unfiltered counts accounts for the complete profile without changing
root denominators. No dependency records are silently excluded.

Dependency counts in this report describe execution during the root test suite.
They are **not full fork-suite evidence** and do not replace independent complete
IMAP/SMTP test runs. Their inherited/upstream code policy remains separate from
the root handwritten floor ratchet. Root-only `-floors` can be combined with the
same two mapping flags; dependency package floors are not required or inferred.

To enforce a reviewed manifest:

```sh
go run ./internal/devtools/coverage \
  -repo . \
  -profile /tmp/fry-foundation-coverage.out \
  -floors /path/to/reviewed-coverage-floors.json \
  > /tmp/fry-foundation-coverage.json
```

The manifest schema is a single object with a `packages` object mapping exact
package import paths to handwritten percentage floors (JSON numbers in 0..100).
Every measured root package must be tracked and every tracked package must appear
in the root report. Mapped dependency packages are outside the root floor scope;
adding a dependency to the root manifest fails as a missing tracked root package. Add new packages and review renames/moves explicitly; do not discard
a disappearing package or lower its floor automatically. Duplicate keys, null
values, unknown fields, trailing data and an empty manifest fail. The manifest
contains no implicit default floor. A generated-only measured package must still
be tracked with an explicit zero floor; a positive floor fails when there are no
handwritten statements.

With valid inputs, a failed floor/package check still emits the full report with
a sorted `violations` array, prints a diagnostic on stderr, and exits nonzero.
Successful reports omit `violations`. Parse/read/output errors also exit nonzero.
The reporter itself exits 1 (2 if writing the error diagnostic also fails); `go run` may add its own exit-status diagnostic.
There is deliberately no baseline-writing or ratchet-update command: acceptance
of numerical floors remains a review step. Package floors do not establish
failure-path coverage or replace the required named regression cases.

Focused verification:

```sh
go test ./internal/devtools/coverage
```
