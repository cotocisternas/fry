# Advisory storage accounting and quota alerts

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Component tests, the full PostgreSQL race suite, vet, the process rehearsal and
local Chromium browser verification passed. Isolated acceptance measurements are
recorded below, including an unexplained slow sample. The matching bounded
SMTP/IMAP workload also passed. This is not production-capacity evidence.

## What the number means

An organization is charged one original's raw email size when its receipt is
durably accepted. It remains counted until active-storage cleanup records that
receipt as removed. Mailbox aliases, distribution destinations, IMAP COPY and
storage replicas do not multiply that original. Incoming SMTP, outgoing queued
mail, IMAP collection, APPEND and staged history imports all use this rule.

The total includes recovery copies and originals awaiting physical cleanup.
Deleting the final visible mailbox copy therefore need not reduce the total
immediately. Preparing or aborted attempts are excluded. These are **logical
retained original bytes**, not disk/S3 consumption: replica duplication, packs,
staging, metadata, abandoned writes and filesystem overhead are not measured.
Quota usage is also not a count of currently visible messages or unexpired
references; lifecycle reconciliation can lag a reference's expiry.

Acceptance and final removal update the counters in the same transaction as the
receipt transition. Successful replay adds nothing; rollback changes neither
receipt nor usage. Lost commit replies are resolved against receipt authority.
Removal uses the actual changed row and fails on counter underflow or a missing
stripe. It does not estimate reclaimed bytes from a scheduled cleanup request.

Counters use 64 stable receipt-hash stripes per organization. The write is the
last locking operation after existing receipt/folder/migration guards; deadline
checks run after that possible wait. Reads sum at most 64 rows in one snapshot.
This avoids a single organization counter lock and whole-history scans per
request, while still adding a write/WAL cost that must be measured.

## Advisory policy and administration

Thresholds start disabled. Global operators and managers assigned to the
organization may read usage and configure a threshold without content access.
Mutations require the existing session, Origin/CSRF checks and transactional
audit. Counts and byte quantities are decimal strings in JSON, preserving values
beyond JavaScript's exact-number range. A threshold is a positive signed-64-bit
byte count; explicit `null` disables it. The service never silently treats an
invalid or omitted threshold as a disable request.

```sh
fry-admin -organization "$ORG_ID" storage-usage
fry-admin -organization "$ORG_ID" -limit-bytes 10737418240 storage-quota-set
fry-admin -organization "$ORG_ID" -disable-limit storage-quota-set
```

Supply administrator authentication through `FRY_ADMIN_PASSWORD` and the usual
`-origin`/username flags. The Storage web tab exposes the same scoped controls.
The HTTP endpoints are `GET /api/storage-usage?organization=…` and
`POST /api/storage-quota` with `{organization, limit_bytes}`.

An alert is active when sampled retained bytes are **greater than** the threshold;
equality is within the limit. Policy changes schedule evaluation immediately.
The background evaluator claims only an organization's due policy row with
`SKIP LOCKED`, reads committed usage, and atomically records its sample and any
active/resolved transition. It neither locks usage stripes nor alters receipt
or storage authority. Disabling schedules one final evaluation to resolve an
existing alert. Enabled policies are due again after 30 seconds.

The runtime worker evaluates up to 64 due policies per call, immediately
continues a full successful batch, and otherwise waits for its one-second tick.
Calls have a ten-second context bound; shutdown cancels and joins the worker.
Errors preserve prior alert state and retry, with logs on failure/recovery rather
than one error log per tick. The worker currently runs with the administration
service. Disabling that service pauses alert sampling.

Current usage and the last alert sample are separate observations. Responses
include sample time and policy version; an absent, old or superseded sample is
explicitly marked stale. Poll intervals are not a latency guarantee under backlog
or outages. Alerts are durable local administration records; external email,
webhook or paging notification delivery is not implemented in this slice.

**Neither the threshold nor an active alert gates SMTP acceptance or collection.**
Shared physical-capacity and durability protections still apply independently.
This does not reserve space for an organization or automatically delete its mail.

## Schema and verification scope

This adds a fresh-schema accounting version and definition check. An older or
incompatible schema fails startup rather than claiming zero historical usage.
Administration also checks its required quota schema. `-init` remains fresh
initialization, not an upgrade or historical backfill mechanism.

Focused tests independently reconcile counters against authoritative receipts,
including cross-organization and same-stripe concurrency, all receipt paths,
replay, rollback, ambiguous commits, pinned cleanup, APPEND/Expunge, and deadline
expiry while waiting for a stripe. A transaction-aware reader avoids checking
out another connection while an authorized transaction holds the only available
connection. Quota tests cover scoped permissions, audit, exact decimal input,
snapshot consistency, repeated/concurrent evaluation, disable/equality, failed
evaluation, and continued acceptance while a quota policy is locked. A populated
SQL-plan check verifies indexed due selection rather than a full future-policy
scan.

The disposable process rehearsal is:

```sh
./scripts/smoke.sh storage-quotas
```

It checks real CLI configuration, background evaluation, above-threshold SMTP
receipt and exact IMAP bodies, COPY without double charging, recovery retention
after Expunge, alert resolution and clean restart. It reuses the credential
smoke's synthetic fixture setup, not its credential-specific verification.
No cloud resource, external recipient or existing mailbox is used.

The final full PostgreSQL race suite passed (run 95940), as did `go vet ./...`.
The final process rehearsal 85345 passed after checked cleanup: three exact
originals totaling 3,093 bytes and one clean restart. It checked real CLI controls,
background transitions, continued over-threshold SMTP receipt, exact IMAP bytes,
COPY/Expunge accounting and persistence.

Local Playwright/system Chromium browser run 58601 passed on a fresh disposable
demo: two seeded originals totaled exactly 271 bytes as decimal strings; default
disabled policy, threshold 1 active/fresh sampling, equality resolution, disabling
with an invalid number draft, exact 9007199254740993 transmission/display, and
held mutation completion after organization switch were verified. There were
zero page JavaScript errors. Native preview status/open worked, but two snapshot
attempts each failed `Preview automation snapshot timed out after 15000ms.`;
the authorized local fallback completed verification. Browser role was global
operator; multi-organization managers without content permission were covered
by backend tests. The browser was closed and root checked cleanup of its demo
files/database; the independent old demo was untouched.

## Isolated measurement filesystem qualification

The historical isolated acceptance artifacts under `/tmp/fry-acceptance-*`
used fixture scratch under their output paths through the runner's `TMPDIR`.
Current mount inspection identifies `/tmp` as tmpfs on this host. Memory-backed
hot-member placement is therefore inferred from those paths, the runner and the
current mount, rather than recorded per-run filesystem metadata. These runs do
not establish persistent hot-disk fsync performance; their disposable Docker
PostgreSQL storage was separate. See the
[acceptance method qualification](acceptance-profile.md#historical-fixture-filesystem-qualification).

The historical numbers, hashes and unexplained slow outlier are retained below.
Filesystem placement does not explain that outlier without further evidence.
Full SMTP tests used repository-local Btrfs and are a separate disk-backed case.

## Isolated acceptance measurements

All four runs used the same public `Store.Accept` boundary: 1,024×64KiB,
16 workers, a prewarmed 16-connection pool and one local pair on shared storage.
Every run verified all 1,024 receipt identities and complete bodies outside the
acceptance interval and checked fixture cleanup. Baseline has no SQL/runtime
profiling; diagnostic profile enables CPU/block/mutex and SQL/acquire tracing.
See [acceptance-profile.md](acceptance-profile.md) for method and environment.

| Sample | Mode | Elapsed ms | Accepted/s | p99 ms |
| --- | --- | ---: | ---: | ---: |
| Before accounting | Baseline | 999.519 | 1,024.493 | 25.089 |
| Accounting first (retained slow observation) | Baseline | 4,920.422 | 208.112 | 209.542 |
| Accounting diagnostic, unchanged production | Profile | 919.153 | 1,114.069 | 25.426 |
| Accounting repeat, unchanged production | Baseline | 908.348 | 1,127.322 | 25.382 |

The first accounting observation was almost five times slower; it is retained,
not discarded or called a fixed bug. The unchanged repeat and diagnostic did
not reproduce that slowdown. No reproducible fivefold regression or cause has
been established. These few short samples establish neither an overhead bound
nor a speedup. Diagnostic timing is not interchangeable with untraced baseline
or a controlled estimate of profiling overhead.

The diagnostic fingerprint
`dbedb9aa7ab26edc6dd4bc0ba5a56f7e80a0d68d8ba572e421f5ab61235eba80`
corresponds to the accounting UPSERT: 1,024 calls, 127,214,828ns accumulated
(0.124ms mean). There were 4,096 commit calls accumulating 8,653,455,762ns.
Concurrent durations overlap; these are not an additive wall-time breakdown or
proof that accounting cannot contend under another workload.

Local evidence roots (mode child contains `report.json`):
`/tmp/fry-acceptance-before-accounting-1/baseline`,
`/tmp/fry-acceptance-with-accounting-1/baseline`,
`/tmp/fry-acceptance-accounting-profile-1/profile`, and
`/tmp/fry-acceptance-with-accounting-2/baseline`.

| Report, in that order | SHA-256 |
| --- | --- |
| Before | `435b8fd38ee4449817655e55efc048d2a1f917b684977fd4308f8f7ac8361142` |
| First after | `4c048e2b0df5270f8910b601526e0cc980a68c570715a87994ddd339e820f9d3` |
| Diagnostic after | `4ce22b5a35ac03ebda41830bf383decafa8523ee3fa264d64dad5ddc856da93a` |
| Repeat after | `f25cc79fdf55fc20dd0bb9aa5ccb18bec22c41e2184af98b4ab5afecb51db1ad` |

The three after reports have identical source hashes. Core snapshot:
`storage_usage_linux.go`=`8555eadd254c009f123448905271d12b2194da179d27ecbe44b9489de6ec42f3`,
`postgres.go`=`daf0cee42e3f385b39f54f195ee74c60ac18dda561ab3d641ab01b50f4ad22c9`,
`store_linux.go`=`01b0fa8bd639696fa813408f59eafb5629e3b508428cc51c1ddbcf0e7f44f69a`,
`schema.sql`=`c52b6cf58942b5f95c347e95dac0a1cade423d9bfe9ad4de0de28fb07d713f07`.
The after measurement test hash is
`d802a0e717821442725d608d7e28223564f0fdc7c86f1ce42744cd2eb66688bb`;
before is `3988365c37e1496713404ae1b36c310b99038e2a8b63f72fee4742d9a9d8bae1`.
Full source identities remain in each report. The before schema was
`b62b4b2a6d3ec9b378bed339826eefb76a95ea9c99a1728c6ce48b31b9320c22`.

## Complete bounded SMTP/IMAP observation

The full workload 49177 passed with exit 0, using 5,000×65,536-byte messages,
16 workers and one local pair. The actual report is
`/tmp/fry-load-5000-accounting.json`, SHA-256
`e726b409d8ae58201b1f334ead4bf98d6677588c612903e38410a25c53bb28da`.

| Metric | Observed |
| --- | ---: |
| Accepted / deferred / failed | 5,000 /0 /0 |
| SMTP elapsed | 11.219417s |
| Acceptance rate | 445.655972/s |
| Acceptance p50 /p95 /p99 | 35.636841 /44.854875 /50.086023ms |
| Last acceptance → all-visible | 2,909.875631ms |
| First acceptance → all-visible, direct timestamps | 14,098.206520ms |
| Combined header/date/unseen SEARCH, cold /warm | 9,781.079529 /250.532618ms |
| All-message header/metadata FETCH | 267.944723ms |
| Headers /complete-and-partial original samples verified | 5,000 /8 |
| FRY peak RSS, PostgreSQL/client excluded | 45,284KiB |

This run verified every header and metadata/flags plus eight full and partial
original samples, **not all 5,000 bodies**. Publication/SEARCH observations and
Go/process memory scopes follow the [load harness](local-load.md). These are
individual synthetic local results; neither the lower cold SEARCH time nor
acceptance rate relative to earlier runs proves an accounting-induced change.
They do not remove the unexplained isolated outlier or establish a production
capacity/overhead bound.
No production throughput, tenant latency isolation, physical space accounting,
notification SLA, historical backfill or distributed availability guarantee is
established by these local checks.


The [offline restore follow-up](offline-restore.md#follow-up-with-storage-accounting)
also passed with the populated usage counters included in its full database/
sequence checkpoint. It verified five saved originals through both replicas,
one new original and one restored queued handoff. Quota policies/alerts were
empty defaults, so configured-policy or active-alert restoration is not claimed.
