# Running storage lifecycle maintenance

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

The coordinator can now schedule the existing expiry, packing, compaction, retirement, and physical-cleanup operations. Remote storage members can reclaim their hot files through authenticated maintenance requests. The separate-process smoke exercises the combined path with real PostgreSQL, member/coordinator executables, and a narrow local HTTP object-store fixture. Real AWS permissions, performance, and AZ independence remain unverified.

## Configuration

Add `storage_lifecycle` to the coordinator configuration. The object settings below illustrate an AWS deployment; replace the account ID and provide the named credential profiles. No keys are embedded in FRY JSON. An empty producer profile uses the SDK default credential chain, including configured web identity; the retirement profile must be explicit and different from the producer profile label.

```json
{
  "storage_lifecycle": {
    "objects": {
      "region": "sa-east-1",
      "bucket": "your-dedicated-fry-pack-bucket",
      "expected_bucket_owner": "123456789012",
      "producer_profile": "fry-producer",
      "retirement_profile": "fry-retirement"
    },
    "scratch_directory": "/var/lib/fry/pack-scratch",
    "max_pack_bytes": 268435456,
    "max_staging_bytes": 1073741824,
    "pack_lease_seconds": 300,
    "interval_seconds": 5,
    "work_timeout_seconds": 300,
    "max_actions": 5,
    "max_receipts": 1024,
    "reclaim_hot": true,
    "rotate_hot": false,
    "reclaim_interval_seconds": 60
  }
}
```

This block assumes the configured largest raw message, including framing, fits the pack limit. The 200 MiB prototype limit fits a 256 MiB pack. Scratch must cover the pack image and concurrent record reservations; `NewPacker` enforces that bound. Each coordinator requires exclusive ownership of its own durable scratch directory. A restart reconciles that directory's tracked jobs before scheduling new work.

`reclaim_hot` schedules alternating complete member sweeps over current and sealed files. Optional `rotate_hot: true` requires reclamation and adds a third sweep for [conditional automatic rotation](../design/automatic-hot-rotation.md). Each opportunity uses the same `reclaim_interval_seconds`; enabling rotation changes how often each class receives a turn. Rotation defaults off, applies only to bounded profiles, and skips busy members without waiting for their local reader/writer gate. Continued readers, unresolved attempts or insufficient physical capacity can still prevent progress.

For compatible object storage, configure an explicit HTTPS `endpoint` and, if required, `use_path_style`. Certificates and hostnames use normal verification. HTTP and anonymous credentials require `loopback_anonymous: true`, a numeric loopback endpoint, and path-style access; this is for disposable fixtures. Ambient SDK endpoint environment variables cannot override FRY's validated endpoint selection. The client uses bounded HTTP requests and retries; every lifecycle pass also has a context deadline.

The bucket must never have been versioned and must have no lifecycle rules. The adapter verifies these current settings at startup. Retirement conditionally replaces a pack with an empty permanent marker, rather than deleting its key. Producer/retirement profiles select credentials; different labels cannot prove distinct effective principals or enforce IAM permissions. See [runtime S3 research](../research/s3-runtime-configuration.md) for exact policy and compatible-service requirements.

Without `storage_lifecycle`, background lifecycle work remains disabled. With the block but without `objects`, the worker can sweep expiry, finish recorded cleanup, and optionally reclaim hot files; it cannot pack or serve cold objects. The coordinator refuses to start with existing live packed receipts and missing object configuration. Startup validates the configured object service, so restarting during an object-service outage currently fails closed. Once running, an upload outage leaves incoming data in hot replicas until safe capacity is exhausted.

The top-level `cleanup_deadline_hours` setting defaults to 24. Its value is captured with each acceptance attempt. A later reclaimer, including another process with different settings, uses that recorded policy to compute the receipt's removal deadline. Policy changes do not silently rewrite earlier receipts' deadlines.

## Immutable object namespace

Before opening or reconciling pack staging, `NewPacker` binds the receipt authority to the object adapter's physical namespace in PostgreSQL. The S3 adapter derives a versioned identity from its actual producer and retirement clients: canonical endpoint, region, path-style setting, bucket, and expected bucket owner. Producer and retirement namespaces must agree. Credentials and profile labels are excluded, so credential rotation does not change the binding. Equivalent endpoint host casing, default ports, and a root slash canonicalize to the same identity; caller-owned endpoint pointers cannot redirect an already constructed adapter.

The singleton binding is established transactionally and survives pack retirement. Concurrent first starts cannot bind different namespaces. An unbound authority with any historical pack record, including a retired pack, is rejected; it cannot silently adopt a configured service as the location of old keys. Later endpoint, bucket, region, addressing-mode, or expected-owner changes fail before staging reconciliation or adapter attachment. This prevents a restart from marking an absent key in a different bucket as retired while the original object's bytes remain elsewhere. There is no automatic adoption or namespace-migration operation.

This is a configured-address guarantee. It cannot detect replacement of a backend behind an unchanged endpoint, bucket recreation, or externally erased/replaced database history. AWS expected-owner checks constrain the account, while compatible services may omit that setting. Unknown custom endpoint resolvers and special ARN, accelerate, dual-stack, and FIPS routing are unsupported and fail closed. HTTP transports and middleware remain trusted deployment code; this identity does not attest service durability, DNS ownership, IAM separation, or physical deletion by the provider.

## Bounded scheduler

`receiptstore.NewMaintenance` provides a `RunOnce` interface. One pass visits each enabled category at most once, bounded by the action count and deadline. New packs contain at most the configured receipt count and use the exact encoded image size, not an estimate from raw bytes. Oversized candidates are reported as a bounded sample and do not conceal smaller eligible mail.

Cold operations rotate between organizations within this worker. This cursor is in memory: it does not prove durable fairness across multiple workers. Expiry retains global ordering. Cleanup advances a separate process-local deadline scan, described below. Selection is advisory; receipt guards, pack leases, staging ownership, read pins, and publication checks remain authoritative. Expected concurrency conflicts wait for another scheduled pass; partial errors preserve pending obligations.

A mixed-live pack is compacted before its old image is retired. Interrupted packs cannot retire until their lease/staging and actual readers permit it. Expired read deadlines do not count as closure. `FinishCleanup` records removal only after all registered hot/object copies and actual reader obligations have been resolved. No timer directly deletes mail that still has a mailbox or other live reference.

## Cleanup progress around blocked receipts

`FinishCleanup(ctx, limit)` examines at most `limit` retiring receipts per call.
Its scan freezes an upper `(cleanup_due, id)` endpoint and advances one candidate
at a time, preserving deadline order within that cycle. Unresolved copies, packs
or read pins therefore cannot keep the oldest receipts in every selected batch.
Newer deadlines wait for the next cycle, allowing repaired older debt to be
revisited even while new mail becomes eligible for cleanup.

The selection gate protects only the in-memory cursor. It is released before
receipt locks and deletion transactions, so a blocked transaction does not hold
the selection gate. The cursor advances only past the candidate actually about
to be attempted: a failure on the first receipt cannot skip an unattempted batch
suffix. Concurrent calls retain their original cycle identity and stop when
another call ends that cycle. Restart resets the cursor; independent Stores can
attempt the same receipt and rely on the existing transactional guards.

Selection grants no deletion authority. Header metadata is deleted on a retiring
receipt's turn even when unresolved byte or reader obligations still prevent the
removed state. The receipt lock, target-copy flags, pack retirement and staging
closure checks, and actual read pins remain authoritative. A removal contributes
to the returned count only after its transaction reports successful commit.

The fresh schema indexes `(cleanup_due, id)` for the endpoint and seek queries.
It is not an upgrade for an existing database. A retiring row without a cleanup
deadline is an invalid authority state and is reported explicitly; the normal
lifecycle transition always records that deadline.

This is progress within a process, not organization fairness or a 24-hour
physical-erasure guarantee. Backdated arrivals within a cycle's remaining range
can extend that cycle; repeated restarts, storage outages, unresolved reads and
database contention can delay cleanup. Receipt and attempt history remain.

## Remote hot reclamation

`Store.ReclaimHot` dispatches to the local adapter or private member transport. The member RPC exposes a stable operation ID and authenticated status so a lost reply can be resolved without inventing completion. The server verifies the process owner and physical incarnation, drains its active byte operations under the exclusive local gate, and commits a durable admission gate before inventorying attempts and receipts. New target preparation checks that gate under the same metadata authority. Existing preparing attempts cause a safe retry instead of rewriting beneath them.

The rewrite retains live unpacked records and every pinned record in the same failure domain. Publication revalidates the snapshot, process owner, and maintenance token; it updates retained locators, manifest versions, and the authoritative physical incarnation atomically. Admission remains blocked until the old file is closed, removed, the directory is synced, and the authoritative replacement is reopened. An uncertain publication freezes the member and retains the tracked generations/gate. Clean close/reopen reconciles them; an unclosed process generation still requires trusted external fencing and cannot silently restart.

Other coordinators refresh physical identity without adopting a changed process-owner epoch. Identity refresh requests are serialized so a delayed old reply cannot overwrite a newer incarnation. Remote reads resolve the current authoritative locator by member/receipt while preserving owner checks and parent/member read pins. A client descriptor created before a rewrite therefore does not permanently lose access to retained mail. Retrying an already accepted attempt verifies its immutable request in PostgreSQL and can return its original receipt even when both member endpoints are unavailable.

The prototype still rewrites one append file per member and pauses that member's byte operations. It loads that file's inventory and may defer when preparing attempts exist. Sealed segment rotation, scalable inventory paging, sustained-load drain behavior, and online rewrite efficiency remain production work. The configured timeout bounds attempts; it does not turn this into a demonstrated high-throughput compactor.

## Validation

```sh
./scripts/tests/receipts.sh -race -short
go test -race ./internal/objectstore
./scripts/smoke.sh replicas
```

The process smoke starts the coordinator, two mTLS member processes, and a disposable HTTP object fixture. It verifies automatic packing and hot reclamation, exact mail reads after both hot copies are removed, clean restart, acceptance and hot reads during an object outage, packing after recovery, staged migration against active remote members, and conservative behavior after abrupt member death. The fixture remains in one machine's memory; it is not an S3 emulator, AWS durability test, or six-hour buffer measurement.

PostgreSQL/S3-fixture tests cover scheduling fairness within a worker, encoded size bounds, interrupted uploads, compaction/retirement, unresolved readers, and cleanup policy snapshots. Namespace regressions cover independent object-service endpoints and bucket drift, reconnecting with the same address, rejection of historical unbound packs, and racing first bindings. Adapter tests cover producer/retirement mismatch, canonical endpoint identity, credential independence, unknown resolvers, and caller-owned endpoint pointer mutation. Remote tests cover preparation racing the gate, two independent cached coordinators, active reads and pins, uncertain publication, lost replies, operation replay/status, clean recovery, and refusal to adopt crashed owners. The separate 200 MiB streaming test runs without race instrumentation for its allocation budget; it is not a sustained-throughput benchmark.

Cleanup scheduling regressions use real accepted originals and hot rewrites.
They cover an oldest receipt blocked by hot copies or an actual open read,
header deletion while those obligations remain, later eligible removal, repaired
debt revisited amid arrivals, equal deadlines, a concurrently removed endpoint,
backdated arrivals, independent Stores and canceled selection. A controlled
PostgreSQL row lock proves another call can progress and that cancellation skips
only the attempted candidate. A deferred database trigger forces commit failure
and verifies the returned removal count remains zero. Focused PostgreSQL race
checks passed alongside the existing pack-staging and both-hot-copy guards;
independent source review found no remaining P1/P2 issues.
The complete PostgreSQL-backed race suite and `go vet ./...` also passed after
integration of the cleanup scan changes.

No real bucket was created. Actual AWS conditional-write/IAM behavior, independent-zone disks, HA metadata, replica repair, automatic reduced-protection mode, cleanup deadline attainment, and sustained organization fairness still need validation. Maintenance/audit/attempt history currently has no pruning policy. Periodic logs report progress and failures; production metrics, alerts, and maintenance controls remain work to complete.

Once an object namespace has been bound, runtime startup requires object configuration even if no live receipt currently points at a pack. Historical pack rows also trigger this guard. This preserves cleanup obligations for interrupted uploads, retiring packs, and permanent markers; removing the configuration is not an implicit storage migration.
