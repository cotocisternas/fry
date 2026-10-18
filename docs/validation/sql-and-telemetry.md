# SQL and telemetry validation

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Local Linux amd64 checks, 2026-10-18, Go 1.27.2 and disposable PostgreSQL 18.6.
See the [design and configuration](../design/sql-and-telemetry.md).

## Scope and regressions

The SQL migration covers 501 named queries across administration, directory and
receipt storage. The existing PostgreSQL tests exercise authorization, nullable
values, mailbox UID/flag semantics, transactions, member ownership, cleanup,
query-plan bounds and storage failure handling. Schema DDL is unchanged.

Independent review found an invalid receipt alias in a cleanup query during
migration; it was corrected before integration validation. The sqlc parser alone
did not catch it, reinforcing the need for actual PostgreSQL execution.

New telemetry tests inspect exported OTLP protobuf for parent/child query spans,
operation metrics, privacy, hostile environment variables, disabled operation,
HTTP streaming completion and exporter outages. Review found opaque `tracestate`
could expose internal header values; instrumentation now removes that state while
preserving trace identity. A sentinel regression checks actual exports.

The member HTTP test fixture now cancels its startup context immediately after
construction. Requests must remain usable until their own cancellation or server
shutdown. This guards against coupling serving lifetime to a startup deadline
when carrying telemetry values into HTTP contexts.

The executable smoke enables telemetry on a real FRY process and local collector.
It verifies incoming mail, authenticated submission, relay handoff, exact IMAP
bytes and clean restart; exported traces contain the expected protocol/database
operations and metrics, without the synthetic body, sender or credentials.

## Reproduction

```sh
make sqlc-check sqlc-vet
./scripts/tests/receipts.sh -race -short
FRY_SMOKE_TELEMETRY=1 ./scripts/smoke.sh smtp
go vet ./...
go -C third_party/go-imap test -race -count=1 ./...
go -C third_party/go-smtp test -race -count=1 ./...
GOOS=linux GOARCH=arm64 go build ./cmd/...
```

Generation comparison and the executable smoke passed. The initial full
PostgreSQL race suite passed, including receiptstore (104.672s), administration
(80.063s), IMAP (67.199s) and the telemetry integration test (1.147s). Both patched
mail-library race suites passed. The final full PostgreSQL race run after the member-context regression also
passed: receiptstore 100.128s, administration 79.541s, IMAP 67.455s and telemetry
1.146s. `go vet ./...`, formatting checks, ShellCheck for both changed scripts,
and the Linux arm64 command cross-build passed. Disposable test containers and
scratch storage were removed.

Session logs (temporary local files):

- `/tmp/fry-otel-sqlc-final-race.log`
- `/tmp/fry-otel-sqlc-generation.log`
- `/tmp/fry-otel-sqlc-smoke.log`
- `/tmp/fry-otel-sqlc-vet.log`
- `/tmp/fry-otel-sqlc-arm64.log`
- `/tmp/fry-sqlc-imap-fork.log` and `/tmp/fry-sqlc-smtp-fork.log`

The short suite excludes the large-message stress cases. Cross-compilation does
not establish native arm64 execution. This evidence does not establish EKS/AWS
behavior, telemetry capacity at production volume, AZ recovery or production
readiness. No AWS resources or external collector services were used.
