# Storage scaling: proposed next implementation slice

Status: the static-catalog slice is implemented as a prototype under [ADR 0016](../adr/0016-freeze-ingestion-placement-catalog.md), with [validation evidence and limits](../validation/static-ingestion-shards.md). The subsequent bounded current/sealed storage slices are also implemented under ADRs 0017–0021: [explicit rotation and sealed reads](../validation/hot-file-rotation.md), [sealed-file reclamation](../validation/sealed-file-reclamation.md), and [conditional automatic rotation](../validation/automatic-hot-rotation.md). These are local prototype results, not guarantees for production throughput, concurrent maintenance or recovery.

This document preserves the original staged proposal and measurement history below. References to a following segmentation slice describe that historical plan; the linked validation documents record the implemented scope. No production topology or shard count is selected here. The direction remains grounded in [replicated hot storage](replicated-hot-storage.md) and the [protocol draft](storage-protocol.md).

Implement a bounded, immutable catalog of ingestion shards, each containing two existing storage members in distinct failure domains. Keep each member's present append-file and reclamation implementation for this slice. This exposes independent append writers to one busy inbox and isolates a member rewrite to its shard without changing the durable-copy protocol at the same time.

## Why this slice first

Before this slice, the [receipt store](../../internal/receiptstore/store_linux.go) contained exactly `[2]Member`: every acceptance used that pair, and `readPinned` locked both local members for its complete read. The implemented catalog now selects one pair per attempt and locks only the relevant members. Within each member, [hotstore.Append](../../internal/hotstore/store_linux.go) still stages concurrently but holds one mutex through copying into `data.fry` and its fsync. Increasing SMTP concurrency alone does not create another append writer.

[ReclaimHot](../../internal/receiptstore/hot_cleanup_linux.go) takes a member-wide exclusive gate, inventories its entire current file, copies survivors, publishes replacement locators, and removes the old generation. The pause grows with retained bytes and record count. [hotstore.Open](../../internal/hotstore/recovery_linux.go) validates every record and builds an in-memory map; `Inventory` and `snapshotHot` create further whole-file collections. These remain real scale limits after sharding.

The [retained-data gate](../validation/retained-storage.md) now records four
warm-cache Open/Inventory cases, including equal 64 MiB payload volumes at
1,024 and 65,536 records, and three actual half-expired whole-member rewrites.
The measured map/snapshot memory and rewrite times show record-count costs;
they do not isolate a rewrite bottleneck, select segment sizes, establish peak
memory or prove production restart/reclamation bounds. Segmentation remains a
proposal with the identity and recovery prerequisites below.

| Candidate | Immediate gain | Invariants that change |
| --- | --- | --- |
| Static pairs using existing members | Independent fsync lanes; ingestion on other shards continues during one shard's maintenance | Placement selection and lookup; existing byte durability and cleanup machinery remain |
| Multiple append segments inside one member | Bounded cleanup units and potential bounded restart/index work | Member/file identity, preregistered write obligations, rotation durability, readers, cancellation, tail repair, rewrite publication, and remote read resolution |

Static pairs are the smaller useful step. Merely putting several shard labels on the same two `LocalMember` instances provides no writer parallelism. Each shard in this slice must have its own two member IDs, roots, and append stores; a member cannot belong to multiple shards. Several members may share a host or disk, but that shares physical capacity and failure risk and must be measured honestly.

This is not the final segmented design. The first milestone is demonstrating independent progress and measuring where the next limit moves, not claiming tens of millions of messages per day.

## Small interface and implementation seam

Keep protocol callers on the existing `Store.Accept`, `Status`, `Open`, reference, packing, and cleanup interfaces. Add only a constructor configuration surface:

```go
type IngestShard struct {
    ID      ID
    Members [2]Member
}

func NewWithShards(
    ctx context.Context,
    pool *pgxpool.Pool,
    shards []IngestShard,
    opts Options,
) (*Store, error)
```

The existing constructors remain one-shard adapters with a documented stable legacy shard ID. `Member` remains sealed to the local and authenticated remote adapters. This is a configuration seam inside the receiptstore module, not a new plugin interface or a router that SMTP must understand.

Bound the first implementation to 1–16 shards; the upper bound is an implementation limit to validate, not sizing advice. Sort by immutable shard ID, reject duplicate member/shard IDs, and require two distinct failure domains and incarnations per pair. Keep a private member-ID lookup and a private placement function. Do not add public choose-shard, rotate-placement, or reroute-attempt methods.

For fresh attempts, use a versioned SHA-256 hash of the server-generated **attempt ID** to select an entry in the frozen ordered catalog. This refines the earlier draft's receipt-ID hash: the current attempt ID already exists before `prepareMembers` generates its receipt ID. It avoids splitting registration into another transaction merely to obtain a routing key. It distributes one inbox's independent submissions without depending on organization, mailbox, or Message-ID. Persist the choice; hashing never reconstructs historical authority.

The implementation selects attempt-ID hashing and persists placement version 1; it does not add an intermediate receipt-ID reservation transaction.

### Durable placement

Add a singleton placement version/algorithm and catalog rows binding shard IDs to their two member IDs. Store placement version and shard ID on each attempt. Existing `fry_target` rows remain the exact operation destinations, including incarnation, owner epoch, failure domain, and returned locator.

Constructor binding is atomic and serialized. A conflicting catalog, missing historical member, reordered meaning, changed hash version, or changed shard count fails closed. Bind member IDs, not their current file incarnations: legitimate existing rewrites change incarnations under the current fencing protocol. Compare canonical catalogs rather than configuration order. No automatic adoption of unidentified historical placement.

For a new attempt, choose the shard, refresh only its remote descriptors, and register both targets in the same transaction as the attempt. Revalidate current placement/member authority there. An existing attempt uses its stored shard and targets; do not retarget an expired attempt according to a new catalog or whichever members are healthy. An accepted replay resolves solely through durable authority and its immutable envelope, even if its shard is unavailable.

### Locality requirements

- Replace `lockMembers()` with a private operation that locks only the selected pair, in member-ID order. Locking every configured member recreates the global maintenance stall.
- `readPinned` resolves member IDs from stored target rows. For local copies, take only those local gates in stable order, then reread locators under the gates so a concurrent rewrite cannot leave a stale snapshot. Preserve its existing parent pin and no-fallback-after-output rule. A cold-only read must not wait for an unrelated member rewrite.
- Abort/cancellation addresses the attempt's registered targets, not a hash lookup or a broadcast to every member. Missing/offline targets remain cleanup debt.
- `ReclaimHot(memberID)` and maintenance dispatch resolve exactly that member. Retain the existing generation publication, ambiguous-commit freeze, actual handle closure, and deletion proof.
- Packing continues to call the receiptstore read interface. Pack format, object namespace, retirement markers, retention grouping, and object-key scheme remain unchanged. One pack may contain receipts from several ingestion shards in the same organization and compatible policy.
- Update [replica configuration](../../internal/replicaconfig/storage_linux.go), coordinator startup, and maintenance member enumeration together. A successful multi-shard constructor with a still two-member CLI is not a finished slice.

## Preserved correctness contract

1. A receipt is accepted only after two matching durable copies in distinct domains and the synchronous authority transaction. There is no degraded-mode fallback or substitution of another shard when one pair is busy.
2. Every possible byte write is preregistered; lost replies, expired leases, and interrupted cleanup retain obligations. Terminal attempt states and owner epochs retain their existing meaning.
3. Placement chooses where new bytes go. Stored locators choose where historical bytes are read and cleaned. Neither a configuration reorder nor a shard-count change may reinterpret them.
4. Read pins and actual member-owned streams still protect bytes. Time expiry alone is not proof of closed handles; member reclamation preserves pinned/live survivors and finishes deletion bookkeeping only after removal.
5. Shared-device admission budgets are not multiplied accidentally. Splitting one volume into several member roots requires allocated per-root budgets whose sum includes staging and rewrite headroom. A per-member budget does not prove physical free space or a six-hour outage reserve.

The expected improvement is concurrency and isolation. Aggregate in-memory indexes, per-message fsync cost, PostgreSQL/WAL pressure, pack scheduling, and whole-file scans are not solved by adding members. Do not increase connection limits or database pool sizes blindly to conceal the new bottleneck.

## Migration and rollback

The repository currently initializes fresh schemas; this proposal does not invent an automatic production migration framework.

For the first experiment, use a new isolated database and new member roots with a multi-shard catalog. Retain the existing one-shard fixtures and behavior. For any existing authority, an explicit offline conversion must stop and prove closure of every owner, validate the complete target history against its original pair, record that pair as the legacy shard, and backfill attempt placement without moving bytes or changing locators. Resume initially with that one shard. Unknown or inconsistent history blocks conversion.

Expanding an existing catalog requires a later explicit placement-version transition: preserve the old catalog and targets, direct only new attempts to the new version, and retain routing to every historical member. That transition and rebalancing are outside this first slice. Do not infer old placement from today's configuration.

After multi-shard receipts exist, rollback cannot simply run the old two-member coordinator or remove catalog entries. Retain the new reader/cleanup support and stop new ingestion if needed. Recover through the persisted catalog and exact target inventory; never discard extra roots to make old configuration pass.

## Verification and measurement gates

Exercise the same receiptstore interface used by callers, with real PostgreSQL and real local file durability. Extend authenticated remote tests for the changed routing paths.

| Gate | Required evidence |
| --- | --- |
| Stable routing | Many generated attempts to one inbox use every configured shard; retries and reordered configuration retain the same stored targets; conflicting first binds produce one winner |
| Independent progress | Hold one pair inside rewrite or block its fsync; acceptance and reads on another pair still complete without acquiring the blocked pair's gates |
| Acceptance and replay | Kill a member after sync/before reply; interrupt publication; repeat the attempt; prove one accepted receipt and exactly its registered two destinations |
| Fencing | Restart/replace one member, change owner epochs, and delay old replies; stale writes cannot publish and cannot reroute to another shard |
| Reads and reclamation | Hold a read across rewrite, change locators between candidate lookup and gate acquisition, cancel a remote read, and crash after rewrite publication; preserve bytes until actual closure and reconcile historical debt |
| Catalog and migration | Reject missing old members, changed pairs/count/algorithm, ambiguous legacy history, and mixed coordinator catalogs before dispatching writes |
| Budget and recovery | Saturate one shard, reopen nonempty stores, and reclaim with S3 unavailable; retain accepted hashes, backpressure safely, and report outstanding cleanup accurately |

Compare 1, 2, and 4 pairs using the same total byte budget and stated hardware, then repeat on independently provisioned storage before attributing a speedup to disk parallelism. Record accepted messages/s and bytes/s, p50/p95/p99 acceptance latency, per-shard rejection and fsync latency, memory by retained record count, database transaction/lock/WAL load, rewrite duration, restart duration, and mailbox publication lag. Count completed accepted receipts, not merely attempted sends.

Run a same-inbox workload independently from an even-inbox workload. Sweep concurrency and message sizes, including large originals. Separate empty-store throughput from retained-data and mixed-live cleanup runs. Inject an S3 outage and measure backlog growth and later drain capacity; a short local run does not establish six-hour storage capacity or 24-hour erasure. No particular rate or linear scaling is guaranteed in advance.

## One inbox has an independent metadata limit

[Mailbox publication](../../internal/receiptstore/mailbox_linux.go) currently allocates a UID by locking the folder row, inserting one membership, and incrementing `uidnext` in one transaction per message. Additional byte shards do not parallelize that row. A 500,000-message inbox burst therefore needs separate acceptance and mailbox-visibility measurements. Pending mailbox ownership already keeps accepted mail durable while publication catches up.

The [publication measurements and experiment](../validation/mailbox-publication.md) isolate this path and include 5,000-message SMTP comparisons. Shared inbox-admission guards improved acceptance and exposed publication backlog. Bounded default-INBOX groups then improved final visibility, but groups of 16 repeatedly reduced intake and worsened p99 latency; groups of four did not establish a consistent improvement. Individual publication transactions remain in use, consistent with the priority on intake throughput and tolerance for processing backlog. The reviewed implementation and fault tests are preserved as an unapplied patch. They acquire sorted receipt guards before folder guards, preserve original UID allocation order, permit individual fallback only after confirmed rollback, and treat commit errors as uncertain. A future experiment should measure competing database work and intake effects before changing this path again. Do not reserve UID blocks outside the publication protocol.

Also measure `mailboxRows`, which currently materializes the entire folder, and selected-session memory. Batching UID allocation alone does not make listing or selecting 500,000 messages bounded. Page-oriented metadata and IMAP sequence-state design need their own tests.

## Following slice: segmented reclamation within a member

The [file catalog](member-file-catalog.md) now records exact physical file ownership through whole-member rewrite. Keep the existing meanings: `member` is the logical member, `owner_epoch` fences its process, and `incarnation` is the immutable physical FileID. There is no need to introduce another volume identity. Inserting rollover into `hotstore.Append` alone still breaks preregistered targets and current-file read assumptions.

The implemented [bounded hot-file admission](bounded-hot-admission.md) slice establishes the prerequisite: reserve record/byte capacity before dispatch and preserve conservative occupancy across lost replies and rewrite failure cuts. It keeps one writable file and the existing maintenance gate. This establishes limits and accounting before changing file selection.

The [explicit rotation and sealed-read slice](hot-file-rotation.md) now extends the catalog with sealed files. Seal only after a closed census of every exact-file obligation, including preparing targets and aborted writes whose replies were lost. Register and durably initialize the successor before atomically switching the current pointer; existing targets keep their original FileID. Uncertain publication retains both files and blocks unsafe admission.

Use authoritative locators for bounded sealed-file reads rather than retaining every sealed record in a process-wide map. This defers detection of corruption in untouched bytes compared with eager whole-file startup scanning; full and partial reads must preserve their existing checks. First reclaim one capped sealed file under the member-wide gate. Allowing the active file to accept writes during that reclamation is a later concurrency change with separate lifetime and closure proofs.

Preserve the framed record/locator format. Packing's single-file `Snapshot` remains useful for bounded scratch images; a member-wide multi-file snapshot must not masquerade as that object. Record and byte ceilings need explicit workload evidence, not an assumed production shard count. The retained-data measurements identify costs but do not establish safe production segment sizes.
