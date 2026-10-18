# Script module conversion

The 2026-10-18 follow-up to the [layout cleanup](scripts-layout.md) removes
embedded Python and JavaScript programs from shell scripts. Shell now selects
native tools and dispatches commands; complete Python modules own the demo,
SMTP rehearsal and benchmark workflows. The six benchmark shell wrappers are
replaced by `scripts/bench.sh CASE [args]`. The generator-package check is an
ordinary JavaScript file under `tooling/rpc`.

The [script guide](../../scripts/README.md) distinguishes routine development
commands from opt-in measurements and failure rehearsals. Their different
assertions remain useful; running every scenario is not a daily requirement.

## Regression checks

`make test-scripts` discovers Docker-free Python tests. These exercise package
imports without side effects, configuration validation, telemetry privacy,
process startup failure, resistant descendants, repeated cancellation and
checked teardown. SMTP workflow tests verify that both restart rounds use the
same configuration and that failed database cleanup cannot print success.

Actual SMTP execution exposed a conversion error: regenerating the self-relay
address between rounds conflicted with its persisted binding. The workflow now
retains the original configuration across restart, as the previous script did.

The query-plan harness also contained a stale reference to inline SQL removed
during sqlc adoption. It now reads the generated query constants executed by
production, uses their actual parameter order, and records their source hashes
under `query_source_sha256`. Missing or ambiguous constants fail validation.
The query-plan report's previous `mailbox_source_sha256` field is replaced by
this map; earlier reports remain historical.

## Execution evidence

Local checks use Linux amd64, native Go 1.27.2 and repository-built PostgreSQL
18.6. Logs, reports, the previous-script snapshot and the one-off demo supervisor
are under `.artifacts/scripts-modules/`.

Passed:

- `make test-scripts`: 41 Docker-free tests, including report validation,
  private/atomic publication, cleanup failures and signal handling.
- `make doctor`, `make fmt-check lint`, shell syntax and focused ShellCheck.
- Source inspection found no executable Python/JavaScript heredocs or inline
  interpreter programs in maintained shell scripts. Help-text heredocs remain.
- SMTP rehearsal with two process rounds, plus the telemetry-enabled run's OTLP
  trace, metric and mail-data privacy checks.
- `make demo`: real startup, seeded messages/admin setup, then SIGINT with
  verified removal of temporary data and disposable dependency containers.
- Retained hotstore: 1,024 records of 1,024 bytes, verified open/inventory results
  and checked report publication.
- Retained reclaim: 128 records of 128 bytes, 64 retained originals verified
  directly from the rewritten member and checked fixture removal.
- Mail load: 32 messages of 1,024 bytes, four workers, two replica pairs.
- Acceptance baseline: fixed 1,024 messages of 65,536 bytes with 16 workers.
- Mailbox publication: all four batch limits (1, 16, 64 and 128), each verifying
  1,024 originals before report publication.
- Query plans: all eight cases against the synthetic 20,000-row fixture, with
  no scan-bound or queue-sort warnings.

These runs validate the converted harnesses, not production throughput. Some
ran concurrently on the same development machine. Profile/trace acceptance
modes, larger workloads, deployment rehearsals, browser tests, native arm64
and live AWS were not rerun for this change. No application runtime or
dependency-image recipe changed.
