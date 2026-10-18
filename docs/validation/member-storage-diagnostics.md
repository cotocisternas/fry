# Member-storage diagnostics validation

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Follow-up on 2026-10-18: the Go toolchain baseline exposed an ID-order-dependent
planner regression in the earlier seek. Explicit member equality allowed
PostgreSQL to drop the leading sort key and use the global file-ID index,
filtering 2,113 unrelated rows at the midpoint and all 4,096 at exhaustion.
The query now combines `member <= $1` with `(member,file_id) > ($1,$2)`;
together these restrict results to exactly the requested member and preserve
composite ordering. No schema or planner settings changed. The regression
checks both member orders and initial/middle/tail/end/beyond cursors, compares
exact returned IDs, and requires a bounded index condition, no sorting and zero
filtered rows. All 100 repeated cases passed, followed by the focused storage
race suite and the full PostgreSQL-backed short race suite. See the
[toolchain baseline](../design/engineering-foundation.md#initial-toolchain-baseline).

Status: implemented prototype; focused/full race checks, static checks and the
real-process rehearsal passed on 2026-10-17.

Global operators can inspect one configured member through
`GET /api/member-storage?member=ID&limit=64` or:

```sh
go run ./cmd/fry-admin -origin http://127.0.0.1:8080 \
  -member MEMBER_ID -limit 64 member-storage
```

Use the existing `FRY_ADMIN_PASSWORD` environment variable for CLI authentication.
To fetch another page, pass the returned `next_file` as `-after FILE_ID` (or the
API's `after` parameter). IDs come from the installation's configured members.
The page limit is 1–64 and defaults to 64. All 64-bit numeric response values use
decimal strings, matching existing administration accounting conventions.

## Meaning and limits

The response separates a repeatable-read database snapshot from a later runtime
sample. It reports profile settings, current capacity lanes, the latest durable
owner-generation row, and a bounded file-catalog page. `can_fit_max_record_snapshot`
only describes lane reservations at that database snapshot; it is not an
admission promise. `limits_frozen=false` identifies legacy profile settings that
are not enforced bounded-profile guarantees.

`charged_bytes` and `staging_reserved_bytes` are conservative accounting
obligations, not filesystem usage or available disk space. There is no aggregate
member/S3 total, background collection, filesystem scan or alert delivery. Pages include removed history and can change between
requests; they are not a consistent backup inventory.

Runtime `sampled` supplies the observed flags; `gate_busy` leaves guarded state
unknown. `unreachable` means the owner could not be sampled, including an owner
whose ordinary request admission has stopped. It does not prove clean closure.
`generation_mismatch` reports the bound and observed owner epochs without
adopting the new generation. Malformed or misbound responses are
`invalid_response`. Metadata and runtime timestamps/current-file IDs may differ
during rotation. The whole store inspection is bounded to five seconds, with a
two-second remote sampling bound and earlier caller deadlines taking precedence.

Only global operators may use this surface; organization managers receive no
shared-member topology. The service checks authorization before inspection,
releases the authorization transaction before the metadata query or RPC, and
checks authorization again before returning any result. Diagnostics never change
ownership, clear closure debt, refresh remote routing authority or run maintenance.

See the [design](../design/member-storage-diagnostics.md),
[store snapshot](../../internal/receiptstore/member_storage_linux.go),
[owner sample](../../internal/receiptstore/member_storage_runtime_linux.go), and
[administration authorization](../../internal/admin/memberstorage.go).

## Focused evidence

Run the PostgreSQL-backed checks with:

```sh
./scripts/tests/receipts.sh -race -short -run '^TestMemberStorage'
```

The first run, `74736`, passed administration (`9.997s`), CLI and all runtime
checks, but failed the catalog query-plan assertion. In a planner-only temporary
catalog with 4,096 interleaved rows from two members, the initial file-ID predicate
allowed an index scan to filter by member afterward: it returned 65 rows while
discarding another 65. Log: `/tmp/fry-member-storage-focused.log`. The temporary
table clones catalog constraints/indexes; its rows do not claim real files or
receipt authority.

The query now expresses a composite `(member,file_id)` lower bound and ordering
while retaining exact member equality. For a fixed member this preserves page
semantics. The plan regression still requires the member and cursor in the index
condition, at most 65 returned index rows, no rows removed by filtering, and no
sort or sequential scan. Focused rerun `98665` passed with the revised tuple
seek: receiptstore `2.577s`, administration `9.613s`, CLI `1.013s`. Log:
`/tmp/fry-member-storage-focused2.log`. The matcher accepts the tuple expression
while retaining the member equality, cursor, scan/filter and lookahead bounds;
no index hints, planner settings or schema changes were used to force the plan.

Before the first integration run, review corrected the runtime pool-release
test to use `MaxConns=1`: dedicated owner connections are outside the pool and
cannot be counted as consuming pool slots. The test now requires the real
authenticated runtime handler to query the same one-connection pool after the
metadata snapshot has released it. A separate administration fixture also shares
one pool between its service and receipt store.

The passing focused matrix covers:

- Real bounded acceptance, full versus fitting lane snapshots, legacy unknown
  capacity fields, decimal-string JSON, and exclusion of private paths.
- Mixed current, sealed and removed catalog history, bounded
  keyset pages, and the 4,096-row planner-only composite seek regression.
- Held local and remote gates returning `gate_busy` with unknown guarded fields;
  frozen closure debt remaining visible without changing storage authority.
- A real clean remote-owner restart, generation mismatch, no identity refresh or
  descriptor adoption.
- Global-only service and HTTP access, rejected organization-manager inspection
  before core collection, CLI query/bounds handling, and revocation during
  inspection returning no snapshot.
- Single-connection pool checks at both administration/core entry and the real
  authenticated owner RPC, after releasing the metadata transaction.

Test sources:
[core snapshots](../../internal/receiptstore/member_storage_linux_test.go),
[independent runtime/query checks](../../internal/receiptstore/member_storage_independent_linux_test.go),
[administration](../../internal/admin/memberstorage_test.go), and
[CLI](../../cmd/fry-admin/memberstorage_test.go).

The complete `./scripts/tests/receipts.sh -race -short` run passed, session
`52695`: receiptstore `101.748s`, administration `76.826s`, SMTP `44.800s`
(`/tmp/fry-member-storage-race.log`). `go vet ./...` passed
(`/tmp/fry-member-storage-vet.log`). The short suite excludes the separate
200 MiB stress case. Independent implementation and test review found no blocker.

The disposable process runner is `./scripts/smoke.sh member-storage`. It uses
actual CLI authentication, API requests and two mTLS storage processes to inspect
full bounded files, paginate after real rotation, distinguish a stopped owner
from a new owner generation, and verify explicit coordinator reconnection and
old/new originals. Session `86726` passed with checked cleanup
(`/tmp/fry-member-storage-runtime.log`): two accepted originals, two configured
members, one-row catalog pages, unchanged storage authority after diagnostics,
an unreachable stopped owner, and two consecutive generation-mismatch samples
without descriptor adoption. Explicit coordinator restart bound the new owner;
both old and new exact originals remained accessible. Three owner generations
closed normally, with no outstanding read ownership or application DB sessions.

Validated binaries:

```text
fry          d850259f32267adab1afa683d39be725b0ff1d10f25ad1101be926be466ba4c9
fry-admin    ca860183a937c3596608185077db962bd64d0362607d45d16ffd5a76ce8677ad
fry-member   e9a5f6903431a286f24bc04f8e926c8abdf308e1f28c455a46aaca0125b41505
```

The runtime JSON report also records hashes of the store, transport route,
administration/CLI and fixture source files. This local synthetic test makes no
production scale, cloud failover, actual disk-usage or cold-storage claim.

## Web administration evidence — 2026-10-18

Global operators can now open **Member storage**, enter an ID from the deployment
configuration, and inspect the existing API result without choosing an
organization. Metadata and runtime observations stay separate. File history uses
64-row keyset pages; Refresh restarts from the first page. This is manual
inspection, not physical usage monitoring or a health dashboard.

The new regression follows the repository's existing Playwright/Chromium workflow:

```sh
FRY_DEMO_HTTP_PORT=8087 make demo
# In another terminal, while the disposable demo is running:
FRY_DEMO_ORIGIN=http://127.0.0.1:8087 \
  PLAYWRIGHT_MODULE=/path/to/playwright \
  node scripts/browser/member-storage.cjs
```

The runner installs nothing. It creates synthetic administrator accounts and an
organization in the disposable demo only; stopping the demo removes its data.
Real authenticated API calls verify a global operator receives a snapshot and a
manager with multiple organization/content grants receives HTTP 403. The browser
renders an actual local member response. Intercepted responses then verify 19
exact decimal counters above JavaScript's safe integer range, unknown guarded
fields for busy/unreachable/mismatched/invalid runtime samples, a 64-row first
page and cursor continuation, refresh reset, recovery from lookup errors, and
late responses after navigation or session changes. Those intercepted responses
exercise rendering and browser ordering, not actual member failures or storage
capacity. Actual runtime fault evidence remains the process rehearsal above.

A delayed real logout response checks that sensitive results disappear
immediately while login stays disabled until the expired session cookie arrives.
An old diagnostic HTTP 401 cannot sign out a newer manager session; old successful
responses cannot repopulate another view. The existing administration regression
also passes after the shared table and session changes.

Evidence: new browser regression session `6171` passed
(`/tmp/fry-member-storage-ui.log`), existing administration regression session
`26887` passed (`/tmp/fry-member-storage-ui-admin-regression.log`), both with no
JavaScript page errors. The demo built the current embedded assets and executable.
A separate T3 collaborative-browser walkthrough loaded actual metadata/runtime
sections with no horizontal document overflow at 1280px. JavaScript syntax checks
and independent static review passed. The disposable process, database container
and data directory were removed and port 8087 was no longer listening. No AWS
account, credentials or resources were used. The Go backend is unchanged; this
entry does not claim a fresh full Go race-suite or container-image validation.

Validated source SHA-256:

```text
app.js       da0bc9a9b3f6c15cf1e966063e040c7fb624641958b5dcc61fb31d85b88a5e0e
index.html   b8df4fdda5ef9b4fb40c3928799b35efbe8ff5b20b8940335a58d0b5898debc2
style.css    d1bf62199765bc054e00b7503d5cea2f0abedbc092336eea5d1d6d43d0b1eef9
UI runner    72d16260000d46ce1624cca1a7d513bd4dae252d84a8e863727a2163f6f4dd3e
```
