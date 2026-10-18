# Mailbox publication organization turns

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Validated 2026-10-17 with a dedicated disposable PostgreSQL fixture. The final
exact-source run exited 0, including explicit scan/sort
assertions. No application Store, message body, member file, SMTP load, AWS
resource or existing database was used. These are synthetic authority-row plan
measurements, not storage acceptance or production throughput evidence.

Reproduce with a **new** report filename:

```sh
scripts/bench.sh query-plans /tmp/fry-mailbox-plans.json
```

The runner uses the repository's complete fresh receipt schema and extracts the
actual candidate/wrap/batch SQL raw strings from `mailbox_linux.go`, including its
shared eligibility clause. It fails if that source shape changes rather than
silently testing a separately optimized query. Reports contain complete
`EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)` output, exact SQL/parameters, PostgreSQL
version, and source/schema hashes. Every EXPLAIN is in a read-only transaction
with a three-second statement timeout; setup statements and subprocesses also
have bounded deadlines. Only its uniquely named database container is removed.

## Publication behavior and correctness

`PublishMailboxes(ctx, limit)` selects one eligible organization per call, then
at most `limit` ready destinations from that organization. A process-local cursor
rotates through organization IDs and wraps at the end. The selection gate is
context-aware and released before any receipt or folder mutation. The cursor
advances before fetching the batch, so publication failures and later timeouts
do not give the same organization the next turn automatically. The executable
limits each call to 30 seconds and normally requests 128 destinations.

Availability-first ordering places delayed retries behind earlier-ready work.
Receipt timestamps remain unchanged. Each destination uses its own transaction
for receipt validation, folder UID allocation, mailbox membership and transfer
of the pending reference. A [bounded grouping experiment](mailbox-publication.md)
was not adopted because of its observed intake tradeoff. Selection is advisory:
concurrent workers
may choose the same work, and idempotent publication prevents duplicate mail.
The schema copies scheduling organization/time from the authoritative receipt at
acceptance; query and publication guards still verify organization ownership.

The old global oldest-first selection failed a deterministic backlog regression:
a busy organization received the second turn while smaller organizations waited.
PostgreSQL race tests now verify A/B/C/A rotation before A drains, retry order,
unchanged receipt dates, monotonic UIDs, missing-folder and released-reference
debt, future availability, cursor wrap and newly eligible work. A held receipt
lock times out without retaining the selection gate; another organization can
publish concurrently. Concurrent calls and independent Store instances preserve
unique memberships and reference transfer. Focused mailbox/snapshot and migration
activation checks also pass.

The complete PostgreSQL-backed repository race suite passed after integration
with persistent SMTP credentials and the administration transport regression:
`./scripts/tests/receipts.sh -race -short`. This checks functional coexistence;
it is not a production-rate load test.

## Fixture and measured result

The pinned PostgreSQL image was
`postgres@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336`,
reporting PostgreSQL 18.6, Linux x86_64. There were 20,000 work rows across 21
organizations: one had 18,000 rows, including a 17,000-row oldest-receipt prefix
with future retry availability and 1,000 due rows; the other 20 had 100 due rows
each. The cutoff was fixed at `2026-10-17 12:00:00+00`, with batch limit 64.
All receipt/inbox authority guards were valid, and no inbox was in migration.
Synthetic accepted receipt rows had zero-size placeholder originals and no
physical copies: they exist solely to exercise joins/indexes, never to claim
that mail was durably accepted.

The final index is:

```sql
CREATE INDEX fry_work_organization_available
ON fry_work(organization,available_at,received_at,receipt,inbox);
```

| Exact-source query | Work rows visited | Receipt probes | Shared buffer hits | Execution ms | Queue sort |
|---|---:|---:|---:|---:|---|
| Candidate at start, custom | 1 | 1 | 11 | 0.102 | None |
| Candidate at start, generic | 1 | 1 | 11 | 0.110 | None |
| Candidate after dominant org | 1 | 1 | 10 | 0.152 | None |
| Candidate past end | 0 | 0 | 5 | 0.053 | None |
| Wrap candidate | 1 | 1 | 11 | 0.094 | None |
| Dominant org batch, custom | 64 | 64 | 264 | 0.437 | None |
| Dominant org batch, generic | 64 | 64 | 264 | 0.449 | None |
| Small org batch, custom | 64 | 64 | 263 | 0.434 | None |

Custom and generic plans were selected explicitly with `plan_cache_mode`, rather
than assuming the first prepared execution represents both. All reported shared
read blocks were zero: these are warm synthetic buffer-cache results. Buffer
hits include repeated logical accesses and must not be read as physical bytes
or distinct pages. Timings are single observations, not percentile benchmarks.
The after-dominant candidate sorted the tiny **21-row inbox dimension**, using
26 KiB; it did not sort the work queue. That sort remains visible in the report
and is not treated as a queue-sort failure.

The final batch preserved the ordered work scan with a correlated authority
check:

```sql
SELECT w.receipt,w.inbox,w.organization
FROM fry_work w
WHERE w.organization=$1 AND w.available_at<=$2
  AND EXISTS (
    SELECT 1 FROM fry_receipt r
    JOIN fry_inbox i ON i.id=w.inbox AND i.organization=r.organization
    WHERE r.id=w.receipt AND r.organization=w.organization
      AND i.migration_job IS NULL
  )
ORDER BY w.available_at,w.received_at,w.receipt,w.inbox
LIMIT $3;
```

Snapshot hashes from the final report:

- `mailbox_linux.go`: `5b84e9036d18e620f73a0ea2b4cfad336932ef0b25cca9e5800477108376e8f7`
- `schema.sql`: `423be5fb0f3e2ce0fc6a4018b720492c5fdd8a5e224c49e0764c54409eb28466`

These hashes identify the original query-plan run. The grouping experiment left
selection SQL unchanged and was reverted; subsequent cleanup indexing changes
mean the historical schema hash is not the current complete schema hash.

## Regression caught during measurement

The first organization-scoped query ordered by receipt time and joined authority
tables directly. Its dominant batch joined all 1,000 due work/receipt rows and
used top-N heapsort to return 64: 3,076 shared hits, 4.952 ms custom and 4.989 ms
generic. Merely changing to availability-first order with the matching index
**still** joined/sorted those 1,000 rows: 3,078 hits, 4.911/5.123 ms. The actual
plan placed the inbox scan outside the work scan; matching the index alone did
not preserve ordered output through those joins.

A bounded query-shape experiment then demonstrated that the correlated EXISTS
retains the same receipt/inbox organization and migration guards while allowing
this valid-row work scan to stop at 64. The author adopted that form; the final
run above extracted it from production source. The harness now exits nonzero
for any sort over a subtree containing work rows, or if this valid fixture's
batch visits more than 64 work rows or makes more than 64 receipt probes. It also
checks that each batch returns exactly 64. Candidate scans/probes are limited to
one for the same fixture. Raw plans remain available through the optional JSON
report for diagnosis; small inbox-only sorts are distinguished from queue sorts. Replaying the captured
original and final actual plans through the current guard logic produced RED
for the 1,000-row/top-N plan and GREEN for the 64-row/no-sort plan without
starting another database.

## Limits and fairness scope

This validates the measured index/query shape at 20,000 synthetic rows, with 21
inboxes and this distribution/statistics. It does not prove tens-of-millions
throughput, burst capacity, production I/O cost or a general latency SLA.
More organizations/inboxes, different statistics, cold data, bloat, contention,
concurrent writes or another PostgreSQL version can change the chosen plan.

The 64-row/probe assertions apply to **valid due rows** in this fixture. Long
prefixes whose receipts/inboxes fail authority checks, or whose inboxes are
staged for migration, can require scanning/probing more than the result limit.
The SQL bounds returned candidates; it does not establish a universal database
CPU or row-read bound. Future retry availability is indexed separately from
receipt age so the measured oldest-future prefix is skipped efficiently.

Organization turns apply only to mailbox publication. Selection and advancement
use a process-local per-Store cursor; process restart resets it, and independent
Stores do not share a durable global turn. Availability-first ordering is an
internal scheduling choice: deferred work may move behind other ready work;
receipt-time delivery order is not promised. IMAP UID allocation remains
monotonic at publication. These plans do not exercise the publication transaction
or prove fairness among competing workers. Outbound delivery and cleanup are
not made organization-fair by this change.
