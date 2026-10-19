# SQL generation and OpenTelemetry

Implemented foundation, October 2026. This does not establish the separate HA,
recovery, throughput or production-readiness gates.

## SQL ownership

PostgreSQL application statements are centralized by domain under `db/queries`:

- `db/queries/admin/queries.sql`: administration, authorization and auditing.
- `db/queries/maildirectory/queries.sql`: incoming directory and credentials.
- `db/queries/receiptstore/`: receipt authority, mailbox state and storage lifecycle.

The root [`sqlc.yaml`](../../sqlc.yaml) is the only generation configuration;
it writes checked-in pgx/v5 `dbgen` packages under each owning module. Runtime
schemas are centralized under `db/schema` and consumed by both installation and
generation.
Application interfaces retain validation, transactions, locks and domain errors;
generated database types are not public API contracts. Schema installation and
schema-ledger/catalog control retain their explicit SQL. Runtime schema DDL is
unchanged by this refactor. The administration catalog stub is analysis-only;
it is never installed in PostgreSQL.

Use the pinned native sqlc 1.31.1 binary through Make after `make setup-tools`:

```sh
make sqlc        # regenerate checked-in Go code
make sqlc-check  # generate in a temporary tree and compare; never edits the source
make sqlc-vet    # sqlc vet hook (no extra policy rules configured yet)
```

Generation requires the [native development prerequisites](../development.md#setup),
not Docker. The binary is built with the pinned Go compiler from the committed
module graph under `tooling/native/sqlc`; `scripts/generate/sqlc.sh` selects it
through the toolchain wrapper. Edit SQL/configuration, not generated Go. After any
SQL change, regenerate and run the real-PostgreSQL tests: static generation does
not prove runtime locks, query plans, null handling or atomicity.

Generated methods serve typed writes, single rows and bounded lists. Unbounded
receipt-storage reads retain streaming adapters over sqlc-exported statements:
they must close rows, check terminal errors and preserve cancellation. sqlc's
usual `:many` method accumulates a complete slice and is unsuitable for those
paths. The single-row owner snapshot
also retains an explicit scanner because sqlc 1.31.1 mis-infers nullable fields
from its lateral join. These adapters are a deliberate limit to generated typing,
not permission to add arbitrary inline application SQL. Transaction-local `SET`
and fixed `LOCK TABLE` controls remain explicit alongside schema installation.

## Telemetry configuration

`fry`, `fry-member` and `fry-migrate` accept an optional top-level JSON block:

```json
{
  "telemetry": {
    "enabled": true,
    "endpoint": "http://127.0.0.1:4318",
    "allow_insecure": true,
    "trace_sample_ratio": 0.1,
    "export_interval_seconds": 30
  }
}
```

This example uses a local OTLP/HTTP collector. Use HTTPS for remote export;
plaintext requires explicit `allow_insecure`. Supply the block alongside the
existing command configuration. Omitting it disables telemetry and creates no
exporters or background export workers. The endpoint is a base URL: FRY appends
`/v1/traces` and `/v1/metrics`, preserving an optional base path. Credentials,
query strings and fragments are rejected. FRY does not take exporter endpoints,
headers, resources or sampling configuration from `OTEL_*` variables. Collector
authentication headers and custom trust roots are not configuration features yet;
use a local collector to forward to an authenticated backend.

Trace sampling defaults to 10%; operation metrics cover all instrumented calls.
The export interval defaults to 30 seconds (allowed range 1–300). Providers are
owned by each process; no global OpenTelemetry provider is replaced. Bounded,
asynchronous exports may drop observations under pressure. Collector availability
is not a mail acceptance or readiness dependency. Shutdown gets a five-second
budget and cannot change the durability result of completed mail operations.

## Instrumented boundaries and data policy

OpenTelemetry Go 1.47.0 exports traces and OTLP metrics. Current boundaries:

- SMTP DATA receipt/submission and IMAP APPEND, FETCH and SEARCH.
- Administration HTTP, including Connect/gRPC requests at the HTTP boundary.
- Authenticated member gRPC over HTTP/2 and coordinator-to-member HTTP transport,
  including streamed response lifetime. These are HTTP transport observations,
  not a complete per-method gRPC status/error instrumentation contract.
- Queries on the process-owned pgx pool, including background work and migration.

HTTP spans contain a fixed operation name, bounded method and status code, not
URL paths or request data. Public administration requests start new traces;
authenticated internal requests propagate W3C trace context without baggage.
Remote sampled parents still respect the local sampling ratio. SQL spans contain
a bounded SQL verb, not query text, arguments, database URLs or row contents.
Resources export only the explicit service name. Error strings, panic values,
message contents, addresses, credentials, organization IDs and mailbox IDs are
not recorded by this instrumentation.

Metrics are `fry.operation.count` and `fry.operation.duration` (seconds), with
bounded operation/outcome and protocol attributes. HTTP status is a span
attribute. Outcomes are `ok`, `error`, `canceled` or `timeout`. Use an OpenTelemetry
Collector to expose these to a Prometheus-compatible backend; FRY does not yet
serve a Prometheus scrape endpoint. This work does not add queue-depth/capacity
metrics, instrument every IMAP verb, trace the administration CLI, or replace the
remaining logging/health-contract work in the engineering foundation.

## Validation

Telemetry tests use a local OTLP protobuf receiver to inspect actual traces and
metrics, including hostile environment variables, stream closure, propagation,
privacy, disabled behavior and shutdown. The PostgreSQL integration suite also
executes a real parameterized query through the instrumented pool and checks
parent/child trace linkage. These checks exercise local software behavior, not a
managed telemetry backend or production load. The optional executable smoke
starts a local collector, verifies mail/submission/IMAP telemetry and checks that
synthetic message contents and credentials do not appear in its exports.

```sh
./scripts/toolchain.sh test-go test -race ./internal/telemetry
./scripts/tests/receipts.sh -race -short
make sqlc-check sqlc-vet
FRY_SMOKE_TELEMETRY=1 ./scripts/smoke.sh smtp
```

See [validation evidence](../validation/sql-and-telemetry.md), [version research](../research/otel-sqlc-foundation.md) and the
[engineering foundation](engineering-foundation.md) for the broader scope.
