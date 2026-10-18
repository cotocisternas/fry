# OpenTelemetry and sqlc foundation

Investigated 2026-10-18 against official releases, tagged source, and official documentation. This note records dependency pins and implementation guidance for FRY's Go 1.27.2 baseline. No installation, generation, build, or tests were performed during this research; compatibility below is inferred from published module requirements and APIs.

## Dependency pins

| Component | Exact recommended pin | Evidence |
| --- | --- | --- |
| OTel API, `trace`, `metric`, `sdk`, `sdk/metric` | `v1.47.0` for each module used | [Latest core release](https://github.com/open-telemetry/opentelemetry-go/releases/tag/v1.47.0), [core module requirements](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/go.mod) |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp` | `v1.47.0` | [Tagged exporter module](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/exporters/otlp/otlptrace/otlptracehttp/go.mod) |
| `go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp` | `v1.47.0` | [Tagged exporter module](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/exporters/otlp/otlpmetric/otlpmetrichttp/go.mod) |
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`, if needed | `v0.72.0` | [Latest contrib release](https://github.com/open-telemetry/opentelemetry-go-contrib/releases/tag/v1.47.0), [tagged otelhttp module](https://github.com/open-telemetry/opentelemetry-go-contrib/blob/instrumentation/net/http/otelhttp/v0.72.0/instrumentation/net/http/otelhttp/go.mod) |
| sqlc generator | `github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1` | [Latest sqlc release](https://github.com/sqlc-dev/sqlc/releases/tag/v1.31.1), [tagged generator module](https://github.com/sqlc-dev/sqlc/blob/v1.31.1/go.mod) |

Core and contrib released these versions on October 2, 2026; sqlc v1.31.1 released April 22, 2026. The inspected modules declare Go 1.26.0; sqlc additionally suggests toolchain go1.26.2. Go 1.27.2 exceeds those requirements. `otelhttp` has a pre-1.0 module version: latest non-prerelease does not imply the same compatibility promise as the stable core SDK. Avoid introducing `otelaws`: the latest contrib release deprecates it. [Core releases](https://github.com/open-telemetry/opentelemetry-go/releases), [contrib release](https://github.com/open-telemetry/opentelemetry-go-contrib/releases/tag/v1.47.0), [sqlc releases](https://github.com/sqlc-dev/sqlc/releases).

## Explicit telemetry ownership

The application should construct and own providers, pass their interfaces into adapters, and shut them down on exit. The SDK exposes `sdktrace.NewTracerProvider`, `sdktrace.WithBatcher`, `sdktrace.WithResource`, `sdktrace.WithSampler`, `sdkmetric.NewMeterProvider`, and `sdkmetric.WithReader`. Both providers expose `Shutdown(ctx)` and `ForceFlush(ctx)`. This supports dependency injection without calling global `otel.SetTracerProvider` or `otel.SetMeterProvider`. Close partially initialized resources if subsequent initialization fails. Give shutdown a fresh bounded context after request processing drains. [Trace provider](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/sdk/trace/provider.go), [metric provider](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/sdk/metric/provider.go), [metric options](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/sdk/metric/config.go).

Recommended FRY default: telemetry disabled unless explicitly enabled. Inject `trace/noop.NewTracerProvider()` and `metric/noop.NewMeterProvider()` in that mode; create no exporters and no background export workers. This is an application policy supported by the official no-op implementations, which emit no telemetry. [Trace no-op](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/trace/noop/noop.go), [metric no-op](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/metric/noop/noop.go).

Use a small resource containing deployment identity such as `service.name`, service version, and an operator-supplied instance identity. `resource.NewWithAttributes` or `resource.NewSchemaless` can construct it without automatic host/process detectors. Be aware that SDK `WithResource` still merges resource environment attributes; operators must keep `OTEL_RESOURCE_ATTRIBUTES` free of mail/account data. [Resource construction](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/sdk/resource/resource.go), [metric resource merge](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/sdk/metric/config.go), [trace resource merge](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/sdk/trace/provider.go).

If HTTP instrumentation is adopted, provide all three options explicitly: `otelhttp.WithTracerProvider(tp)`, `otelhttp.WithMeterProvider(mp)`, and `otelhttp.WithPropagators(propagator)`. Omitted options consult globals. Select propagation explicitly; accepting W3C trace context need not entail accepting arbitrary baggage. `otelhttp.NewHandler` automatically adds request/response attributes, so review its attribute generation before wrapping routes containing sensitive paths. A custom span-name formatter changes the name, not every attribute. [otelhttp configuration](https://github.com/open-telemetry/opentelemetry-go-contrib/blob/instrumentation/net/http/otelhttp/v0.72.0/instrumentation/net/http/otelhttp/config.go), [handler implementation](https://github.com/open-telemetry/opentelemetry-go-contrib/blob/instrumentation/net/http/otelhttp/v0.72.0/instrumentation/net/http/otelhttp/handler.go).

## OTLP export behavior

Recommend OTLP/HTTP protobuf for the initial integration. Construct exporters with `otlptracehttp.New(ctx, options...)` and `otlpmetrichttp.New(ctx, options...)`; attach traces through a batch processor and metrics through `sdkmetric.NewPeriodicReader(exporter, options...)`. The periodic reader defaults to 60 seconds between exports and a 30-second collect/export timeout; explicit `sdkmetric.WithInterval` and `sdkmetric.WithTimeout` can bound application policy. [Trace constructor](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/exporters/otlp/otlptrace/otlptracehttp/exporter.go), [metric constructor](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/exporters/otlp/otlpmetric/otlpmetrichttp/exporter.go), [periodic reader](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/sdk/metric/periodic_reader.go).

For both HTTP exporters, `WithEndpoint` accepts host and port, while `WithEndpointURL` accepts a complete signal-specific URL. **`WithEndpointURL("https://collector:4318")` targets `/`, not the signal path.** Pass `/v1/traces` and `/v1/metrics` explicitly when using that option, or use `WithEndpoint` with the default paths. Validate configured URLs before construction: an invalid URL option retains the default instead of returning a configuration error. Both support `WithHeaders`, `WithTLSClientConfig`, `WithTimeout`, `WithRetry`, and `WithInsecure`; reserve insecure HTTP for an explicitly configured trusted local collector. The exporter default timeout is 10 seconds. Do not wrap exporter HTTP clients in OTel instrumentation; the source warns of recursion. [Trace HTTP options](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/exporters/otlp/otlptrace/otlptracehttp/options.go), [metric HTTP options](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/exporters/otlp/otlpmetric/otlpmetrichttp/config.go).

OTLP/gRPC is also available through `otlptracegrpc` and `otlpmetricgrpc` at `v1.47.0`. Their endpoint defaults use port 4317; they support `WithTLSCredentials`, `WithHeaders`, `WithTimeout`, and `WithGRPCConn`. A supplied gRPC connection remains the caller's responsibility to close. Select one transport deliberately; an HTTP exporter does not become gRPC because a protocol environment variable requests it. [Trace gRPC options](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/exporters/otlp/otlptrace/otlptracegrpc/options.go), [metric gRPC options](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/exporters/otlp/otlpmetric/otlpmetricgrpc/config.go).

Recommended FRY attribute policy: bounded operation names and outcome classes only. Exclude SQL parameters, raw SQL, DSNs, mail bodies/headers, addresses, organization/account/inbox/receipt/message IDs, authentication values, and raw error strings. Classify errors before recording them, because error messages can contain those values. Use route templates rather than request paths. Database semantic conventions make parameter collection opt-in and require sanitization where query text contains literals; FRY can enforce the stricter policy of omitting both. [SQL conventions](https://opentelemetry.io/docs/specs/semconv/db/sql/), [sensitive-data guidance](https://opentelemetry.io/docs/security/handling-sensitive-data/).

## sqlc configuration and transaction integration

A sqlc configuration uses version `"2"`, engine `"postgresql"`, and `sql_package: "pgx/v5"`. `schema` and `queries` accept paths or lists. `emit_interface` generates a `Querier` interface; `omit_unused_structs` limits generated models. This illustrative entry uses FRY's existing schema files, without duplicating or changing their DDL:

```json
{
  "version": "2",
  "sql": [{
    "engine": "postgresql",
    "schema": [
      "internal/admin/schema.sql",
      "internal/receiptstore/schema.sql",
      "internal/maildirectory/schema.sql"
    ],
    "queries": "internal/example/queries.sql",
    "gen": {"go": {
      "package": "exampledb",
      "out": "internal/example/dbgen",
      "sql_package": "pgx/v5",
      "emit_interface": true,
      "omit_unused_structs": true
    }}
  }]
}
```

The output and query paths above are examples, not a proposed shared database package. Keep each adapter's generated query set beside its owner. [Configuration reference](https://docs.sqlc.dev/en/v1.31.1/reference/config.html).

Generated pgx `DBTX` requires `Exec`, `Query`, and `QueryRow`, using pgx return types. `New(db DBTX)` binds a pool/connection-compatible executor; `queries.WithTx(tx pgx.Tx)` returns queries bound to an existing transaction. Preserve transaction ownership, isolation, rollback, and commit-outcome logic in the adapter. This lets generated statements join existing authorization/fencing transactions. Generated code does not start or commit them. [Tagged pgx generator template](https://github.com/sqlc-dev/sqlc/blob/v1.31.1/internal/codegen/golang/templates/pgx/dbCode.tmpl), [transaction guide](https://docs.sqlc.dev/en/v1.31.1/howto/transactions.html).

Use `sqlc.arg(name)` for meaningful parameter names and `sqlc.narg(name)` when a parameter must remain nullable. Explicit PostgreSQL casts help inference for expressions such as CASE, COALESCE, intervals, and overloaded functions. Preserve query semantics when adding annotations/casts; casts are not permission to change null behavior. [Named and nullable parameters](https://docs.sqlc.dev/en/v1.31.1/howto/named_parameters.html).

## Types and combined schema analysis

With pgx/v5, nullable text defaults to `pgtype.Text`, nullable bigint to `pgtype.Int8`, and JSON/JSONB to `[]byte`. Crucially, timestamptz maps to `pgtype.Timestamptz` even for a non-null column; `emit_pointers_for_null_types` does not change that mapping. Native PostgreSQL enums generate named string types and constants; a text column with a CHECK constraint remains a string. [Tagged PostgreSQL type mappings](https://github.com/sqlc-dev/sqlc/blob/v1.31.1/internal/codegen/golang/postgresql_type.go), [datatype guide](https://docs.sqlc.dev/en/v1.31.1/reference/datatypes.html).

Use column overrides when an adapter needs an existing named domain type, or overrides for JSON DTOs if automatic pgx JSON encoding fits the contract. Prefer explicit boundary conversions when importing an owning domain package would introduce a cycle. A `db_type` override applies separately to nullable/non-null columns, so configure both if needed. Column overrides take precedence over database-type overrides. For a non-null timestamptz mapped to `time.Time`, use `db_type: "timestamptz"` (verified with FRY generation; the catalog-prefixed spelling did not apply the override here) with `go_type: "time.Time"`; retain an explicit nullable representation for nullable timestamps. [Override rules](https://docs.sqlc.dev/en/v1.31.1/howto/overrides.html).

sqlc preserves explicit file-list order and updates one catalog statement by statement. Thus the same ordered schema list can serve each adapter's query set, including cross-component tables. Existing `CREATE TABLE`, `ALTER TABLE`, and native enum definitions feed type analysis. The catalog updater ignores statements outside its modeled object types; parsing still happens first, so that does not promise arbitrary PostgreSQL syntax support. Keep seed DML and index/constraint DDL in authoritative schemas; generation is not a migration or a PostgreSQL constraint validator. [Path expansion](https://github.com/sqlc-dev/sqlc/blob/v1.31.1/internal/sql/sqlpath/read.go), [catalog parsing](https://github.com/sqlc-dev/sqlc/blob/v1.31.1/internal/compiler/compile.go), [catalog update dispatch](https://github.com/sqlc-dev/sqlc/blob/v1.31.1/internal/sql/catalog/catalog.go).

Exact support for FRY's CTEs, row locks, SKIP LOCKED, ON CONFLICT, custom catalog queries, and session commands must be established when those statements are migrated. Research alone does not establish generation success. Keep dynamic schema installation/catalog SQL in its owning module when it cannot meaningfully become a fixed generated query. Use `sqlc compile` for syntax/type analysis, `sqlc generate` for output, and `sqlc diff` to check drift when implementation validation is authorized. The CLI accepts `--no-database` and `--no-remote` for local-only generation; the pin must be recorded in the generation command/manifest. [CLI reference](https://docs.sqlc.dev/en/v1.31.1/reference/cli.html).

## Implementation findings

FRY uses per-module `sqlc.yaml` files and a digest-pinned generator via
`scripts/generate/sqlc.sh`. Actual PostgreSQL queries exposed inference limits around
nullable comparisons, row-comparison parameters, computed booleans and lateral
joins; explicit casts, named nullable arguments and result aliases resolve these.
Streaming reads retain exported query constants and explicit scanners.

Instrumentation uses the core SDK directly with narrow HTTP wrappers instead of
`otelhttp`, avoiding automatic URL/header attributes. The SDK merges environment
resources even with an explicit resource; export wrappers restrict resources to
`service.name`. Opaque incoming tracestate is removed while trace IDs and flags
are preserved. These are implementation decisions verified by local export tests,
not features guaranteed by research alone. See
[SQL and telemetry design](../design/sql-and-telemetry.md).
