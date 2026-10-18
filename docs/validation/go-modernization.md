# Go modernization checks

Validated on 2026-10-18, Linux amd64, with the pinned Go 1.27.2 toolchain.
Changes are based on commit `702bb95`.

The GoLand reports exposed a missing CI analyzer suite. The root audit reported
124 `modernize` suggestions. The default suite in the pinned golangci-lint is now
blocking for handwritten production and test code, including the separate
dependency-fixture module. No modernization categories are disabled.

Changes include byte splitting, allocation-free split iteration, typed slice
membership/sorting, reverse iteration, typed error extraction, `WaitGroup.Go`,
and `new(value)`. Trivial pointer factories were removed. Release identity
construction uses `strings.Join` in both the storage member and its maintenance
helper, preserving the existing identity bytes.

## Validation

- The extended policy fixture fails under the previous configuration and passes
  under the new configuration. It checks `stringscut`, `stringsseq`,
  `slicescontains` and `slicessort` individually in production and test files,
  verifies generated-file exclusion, and verifies corrected examples pass.
- `./scripts/quality.sh lint-policy-check` passed.
- `./scripts/quality.sh lint --allow-serial-runners` passed with zero issues.
- `./scripts/quality.sh fmt-check` and `./scripts/test-deps.sh lint` passed.
- `GOMAXPROCS=2 FRY_TEST_TIMEOUT=5m ./scripts/tests/receipts.sh -race -short`
  passed across the root module using real PostgreSQL 18.6. The fixture reported
  successful teardown afterward.
- `GOMAXPROCS=2 ./scripts/test-deps.sh test -timeout 5m` passed its race suite.
- Independent review found no introduced code defects in the full change,
  including goroutine accounting, parsing boundaries, nil/empty distinctions,
  reverse ordering, pointer-copy semantics and both release-identity builders.
- `git diff --check` passed. Generated code, upstream forks and dependency
  manifests were unchanged.

Local logs are in `.artifacts/ide-lint-audit/final-lint.log`,
`root-race-postgres.log` and `testdeps-race.log`.

## Limits

This is local validation, not a new hosted CI result. The short suite does not
establish large-stream/load behavior, live AWS behavior or native arm64 execution.
Coverage percentages were not remeasured in this run.

This does not claim complete GoLand inspection parity. Redundant outer parentheses
in call arguments remain outside the current policy. The separate audit's
`unnecessaryBlock` and other optional style rules were not enabled by this change.
See the [current quality policy](../research/go-quality-policy.md).
