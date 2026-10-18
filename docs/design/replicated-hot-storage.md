# FRY — simple replicated receipt storage and packed objects

Status: preferred architectural direction, with a proposed implementation below. The user selected replicated hot receipt followed by asynchronous packed-object uploads, then clarified that FRY should implement a substantially simpler design inspired by SCRAP, without using SCRAP itself. SCRAP is reference material only. See [ADR 0015](../adr/0015-prefer-replicated-hot-storage-and-packed-objects.md). This supersedes the direct-to-S3 acceptance sequence in the [earlier architecture](proposed-architecture.md).

## Proposed simplification

The follow-on [detailed protocol](storage-protocol.md), [interactive architecture](storage-architecture.html), [failure-validation matrix](../validation/storage-failure-validation.md), and [revised cost estimate](../research/packed-storage-cost-estimate.md) specify and evaluate this proposal. Numerical implementation defaults are provisional; none is measured production sizing.

Use the already-proposed durable PostgreSQL metadata service as the single authority for receipt visibility, ownership, physical locations, and pending work. Implement a small Go storage module for append-only receipt bytes, retrieval, packing, and reclamation. It can run as a dedicated process role in the same FRY codebase; it does not need a general document-storage product or an independent public API.

Do not introduce a separate per-shard Raft state machine, Pebble projection, SCRAP Transaction model, or SCRAP service dependency. The database's own durability and failover remain essential. This reduces the number of independent metadata authorities; it does not remove the need for correct replication, fencing, and crash recovery.

## Receipt path

1. Generate a unique attempt ID and resolve organization-scoped envelope destinations. Hash attempt IDs across a fixed catalog of ingestion shards so a batch aimed at one inbox can use several storage writers; generate the receipt ID within preparation and persist the selected shard and exact targets before byte dispatch. [ADR 0016](../adr/0016-freeze-ingestion-placement-catalog.md) records this refinement of the earlier receipt-ID proposal. Retain exact physical locators so future placement changes do not reinterpret old data.
2. Stream the exact original into append-only hot storage on **two storage members in distinct AZs** as the initial normal-mode candidate. Both confirm durable bytes, record boundaries, length, and integrity information before being counted. Store receipt ID plus checksum/length in each record; torn/uncommitted records must be detectable on restart. Memory buffers and write concurrency remain bounded.
3. Commit the receipt, destinations, both durable byte locators, receipt timestamp, and pending work in one PostgreSQL transaction under the configured database durability profile.
4. Only then return final SMTP success. A byte write without the database commit is an unaccepted/orphan candidate, not an accepted message. Lost replies and ambiguous commits require idempotent recovery by receipt identity; a new sender submission can still be a separate receipt.

Each storage member can own its own append files and return its own offsets; replicas need not maintain identical file layouts. This avoids replicated file-offset state machines. Rebuild local indexes from framed records if needed, while PostgreSQL remains the authority for accepted receipts. Safe orphan reclamation must account for active/ambiguous ingest attempts, not just missing rows at one instant.

Two distinct-AZ copies plus appropriately durable metadata are a design candidate for surviving one AZ loss, not a tested guarantee or final topology. The agreed reduced-protection mode still needs explicit safe activation and recovery rules. If PostgreSQL cannot commit or the required active-profile byte placement cannot be satisfied, defer receipt. Acknowledging bytes only in one pod's local buffer is never a durability mechanism.

## Packing and reads

Workers claim bounded groups of accepted receipts using the proposed SQL work mechanism. They build immutable pack objects, upload them to S3, verify the result, and transactionally publish each message's object key, byte offset, length, and checksum. A pack closes on a configured byte limit or age limit; the detailed protocol proposes 64 MiB or 60 seconds for the first spike, with oversized originals in dedicated packs. These are test defaults, not selected production limits.

The database can initially hold the pack index, avoiding a second mutable index authority or a mandatory separate S3 index object. Keep enough framing/identity information in the pack to verify records; database backup and recovery remain mandatory. Uploads and database publication are separate steps: failed/ambiguous publication must leave hot copies recoverable, with reconciliation for unreferenced uploads. Claim expiration can cause duplicate pack work, so publication must enforce current ownership/generation transactionally and avoid conflicting canonical locations.

Read accepted bodies from a healthy hot copy while available, otherwise use the published pack location for a bounded S3 range read. Cache recent bodies if measurements justify it. Keep records independently readable; do not require full-pack restoration or whole-pack decompression to fetch one message. Preserve original bytes and validate checksums through the chosen read contract. Physical shard distribution does not solve the separate IMAP mailbox-state serialization problem.

Hot copies become reclaimable only after durable pack upload and database publication. Reclaim append files only when all their needed records have safe successors; disk pressure must cause admission backpressure before bytes needed by accepted receipts can be discarded. Repacking from independently laid-out hot files adds I/O and must be included in performance and cost measurements.

## Lifetime and bounded scope

Organization storage quotas are **advisory**. Alert on exceedance and continue incoming SMTP receipt and IMAP collection while shared capacity is safe. Existing mail remains accessible and must not be automatically deleted to enforce the advisory threshold. Distinguish these alerts from physical admission limits and retain the agreed fair service across organizations. Quota byte accounting, alert delivery, and outbound-queue policy require later specification.

The agreed initial hot-storage sizing target is **six hours of incoming mail at the expected byte rate while S3 uploads are unavailable**, configurable. This assumes healthy local replicas and metadata storage. Size usable free capacity for that backlog in addition to normal occupancy, replica overhead, cleanup/repair space, and operational headroom; six hours does not mean accepted mail expires or acceptance stops on a timer. When safe capacity is exhausted, temporarily defer new SMTP receipt and pause IMAP collection. Resuming uploads must provide enough spare throughput to drain the backlog alongside new arrivals. The workload byte rate and validated capacity remain measurement tasks.

FRY's lifecycle authority determines whether a message still has mailbox, delivery/work, or recovery-retention references. Expiration removes logical references; an entirely unreferenced pack is retired through durable cleanup work. The detailed protocol replaces retired S3 bodies with permanent empty marker objects to prevent late create-only uploads from recreating removed content. Mixed-live packs require either retaining their dead bytes temporarily or compacting live records into new packs with safe index publication and reader coordination. Grouping compatible retention policies helps but cannot guarantee that live mailbox messages expire together. Hot append files require their own equivalent survivor-copy and reclamation path.

Keep the first design focused on receipt bytes, checksums, append recovery, placement, pack publication, reads, and reclamation. Optional scanning and business-document classification remain outside storage. The detailed protocol and bounded state model are now documented; actual storage failure tests and throughput evidence are the next validation gates.

Physical removal from active storage must complete within **24 hours after the final required reference ends**, with a configurable deadline. This applies to eligible bytes in hot copies and uploaded packs; dropping a database row alone does not satisfy it. Mixed-live packs need reclamation/compaction scheduled against the oldest applicable cleanup deadline, while preserving remaining live records. Budget rewrite capacity and coordinate readers accordingly. Backup/snapshot erasure was not included in this answer. Track overdue cleanup and alert on failures, including inaccessible storage, rather than reporting logical expiry as completed physical deletion.

## Effect on earlier research and estimates

The [SCRAP fit review](../research/scrap-fit-for-fry.md) is historical reference for lessons, not a backlog of changes to SCRAP. Its fixed slots, size limit, Block format, cold-read behavior, and dependency stack do not become FRY requirements. The earlier individual-object hash prefix layout also does not prescribe the pack key format.

The old one-object-per-message cost estimate remains a comparison baseline. The revised model includes hot-copy capacity, upload lag, packing requests, range-read requests, cross-AZ replication, and rewrite/cleanup traffic. Its illustrative 250 KB scenario is approximately USD 30,525/month, about USD 1,019 above the baseline, with substantial exclusions and unvalidated capacity assumptions. No general net saving, production shard/replica count, or five-minute recovery capability has been established.
