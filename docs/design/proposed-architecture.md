# FRY — proposed first-production architecture

**Historical baseline:** the current direction is [replicated hot storage followed by packed object uploads](replicated-hot-storage.md), recorded in [ADR 0015](../adr/0015-prefer-replicated-hot-storage-and-packed-objects.md). Use the [detailed storage protocol](storage-protocol.md), [current architecture view](storage-architecture.html), and [revised cost model](../research/packed-storage-cost-estimate.md) for the follow-on design. The S3-first acceptance sequence and individual-object layout below remain an earlier baseline for comparison. FRY will develop a simpler implementation inspired by the shard-and-pack pattern; SCRAP is reference material, not a dependency. Infrastructure validation remains outstanding.

Companion: [interactive infrastructure and message-flow diagram](proposed-architecture.html).

Status: **proposal for review and validation**, 2026-10-17. AWS São Paulo, a single-region multi-AZ profile, conventional email first, and the product behavior in [discovery](discovery.md) are agreed. EKS, S3, PostgreSQL, load balancers, process layout, and shard counts below are proposed implementation choices. Nothing has been provisioned or benchmarked.

The proposal replaces shared filesystem mail storage with durable message objects plus transactional mailbox metadata. Go processes can be restarted or replaced without treating their local disks as the authoritative mailbox. This does not make IMAP sessions stateless or remove database scaling work.

## AWS infrastructure

- **One São Paulo VPC across three availability zones**, as a proposed reference layout. Public subnets contain mail-facing Network Load Balancer endpoints, an HTTPS Application Load Balancer for administration, and zonal NAT gateways where required. Private subnets contain FRY's worker nodes and database endpoints. Three zones are a proposal, not an already agreed count.
- **Amazon EKS with workers across the three zones.** Run separately scalable Go process roles for mail protocols, mailbox ownership, collection, background work, and administration. They can share one codebase and image; these do not need to become independent microservice projects. The previous six-node costing example is not production sizing. Surviving nodes must have enough capacity after a zone fails.
- **NLB TCP listeners for SMTP, IMAP, and authenticated SMTP submission.** The public SMTP receiver uses port 25; a private backend port such as 2525 remains a candidate to validate. Proposed client endpoints are IMAPS 993 and submission 587/465; actual legacy-client compatibility may require additional explicitly supported listeners. Keep SMTP STARTTLS handling in FRY through TCP forwarding. NLB source-address behavior, long-lived sessions, draining, and large-message timeouts need tests. [EKS NLB support](https://docs.aws.amazon.com/eks/latest/userguide/network-load-balancing.html).
- **ALB HTTPS for the administration web interface and management API.** CLI uses the management API. This is not the deferred application-delivery API. Local accounts and one installation-wide generic OIDC provider feed explicit role and organization mappings. Organization managers remain limited to assigned organizations; operators administer the deployment. Content-access permission and its audit trail are separate from configuration access.
- **S3 Standard for immutable original email bytes**, accessed from private workers through a gateway endpoint. Original objects remain independent of IMAP flags, moves, and deletion. S3's own multi-AZ durability is included in its service; FRY does not create three application copies merely because there are three AZs. SSE-S3 is a possible initial encryption choice; KMS requirements and costs would be a separate decision. [S3 durability](https://docs.aws.amazon.com/AmazonS3/latest/userguide/DataDurability.html).
- **RDS PostgreSQL Multi-AZ DB instance(s), each with one synchronous standby**, for durable state. Start the feasibility spike with one writer/standby pair. Determine the production number of metadata shards from measurements; a standby is failover capacity, not another write shard. Store message records, organization-scoped recipient resolution, mailbox memberships/folders/UIDs/flags, source checkpoints, leases, durable work, configuration, and audit records. Control/configuration tables may share the initial database, rather than requiring an extra database immediately. [RDS Multi-AZ](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Concepts.MultiAZSingleStandby.html).
- **Secrets management and telemetry:** propose AWS Secrets Manager for relay/source/OIDC credentials, workload-scoped AWS permissions, and CloudWatch metrics/logs with deliberate retention. Do not log full emails or credentials. Metadata content-access events must be durably audited before serving privileged content; the audit mechanism and independent retention need specification. These services are supporting infrastructure, not proof of tenant isolation.

S3 gateway-endpoint traffic avoids routing object transfers through a NAT gateway. External IMAP polling and outbound MTA connections still need their own egress paths; they can incur substantial NAT charges. [VPC pricing](https://aws.amazon.com/vpc/pricing/).

## S3 namespaces and request distribution

### Storage choice remains under evaluation

S3 Standard is the AWS reference candidate, not a settled persistence decision. Compare it with SeaweedFS (small-file volume storage) and Ceph Object Gateway for deployments that operate their own storage. An S3-compatible API does not establish equivalent durability, consistency, failure recovery, or cost. See [storage alternatives research](../research/mail-storage-alternatives.md).

For the S3 baseline, evaluate a bounded, disposable cache of recently ingested/read bodies and persist the metadata needed for routine IMAP operations separately. Cache loss must only cause misses; SMTP acceptance must still wait for durable S3 and receipt metadata. Cache hits may improve body-access latency and repeated GET counts, but cannot remove original PUT charges or internet download traffic. Required header/MIME metadata, cache size, admission/eviction, and hit rate need representative measurements.

A separate optimization candidate is a replicated ingest journal followed by immutable packed objects in S3. A pack index maps each message to an object, byte offset, length, and checksum, enabling range reads. Before S3 upload, accepted bytes and recoverable receipt state must already survive the agreed AZ failure; a local spool is insufficient. Journal acknowledgment, fencing, replay, capacity/backpressure, pack publication, and journal reclamation require an explicit protocol. Retire journal copies only after the object and committed index are recoverable. Keep independently readable records and preserve exact original bytes; whole-pack compression can make single-message reads expensive. Version object locators so individual objects and packed records can coexist.

This packing approach changes the simple object-first acceptance path below and is **not selected**. It adds compaction and reference-lifetime complexity: one long-lived mailbox message can pin a pack, and logical expiry must remain distinct from physical reclamation. Tenant/retention grouping and physical erasure need design. It also does not generally eliminate one GET per random message read or reduce the bytes downloaded by consumers.

In the existing hypothetical 20M/day, 250 KB, 30-day scenario, monthly S3 storage is about USD 5,525, PUTs USD 4,200, and GETs USD 336. Ideal packing of 100 messages per PUT would reduce only that PUT line to USD 42 before multipart, compaction, retries, indexes, and journal costs. The roughly USD 10,175 internet-download line remains unless traffic placement or volume changes. This is arithmetic from the [existing cost inputs](../research/sao-paulo-cost-estimate.md), not a new implementation estimate. Simply packing messages after uploading each one individually does not save their original PUTs.

Recommended evaluation: benchmark S3 plus optional cache against a properly replicated SeaweedFS deployment; include retained-data scale, cold/repeated reads, concentrated bursts, AZ failure, and full operating cost. Retain Ceph as a candidate where operational expertise or existing infrastructure makes it relevant. Only undertake a custom durable journal/packing design if the measured latency or net savings justify its additional failure and lifecycle mechanisms.

### Individual-object key layout candidate

The object-key layout is part of the persistence boundary; protocol implementations must use the stored object locator rather than inventing their own paths.

Propose a private general-purpose bucket for original mail per independent deployment/environment, with versioned, distributed object keys:

```text
s3://<deployment-originals-bucket>/originals/v1/7f/<opaque-receipt-id>.eml
```

Here `7f` is the first byte of SHA-256 over FRY's generated receipt ID, encoded as two hexadecimal digits. It is not derived from the organization, inbox, date, email Message-ID, or message content. Each new receipt can therefore use a different stripe even when a batch targets one inbox. The receipt ID exists before upload, so key selection does not require buffering or hashing the complete email. Retries of the same receipt attempt retain the same key. Persist the exact bucket/key and layout version with the receipt; later layout changes must not silently change how existing objects are located. The 256-stripe choice is a benchmark candidate, not a measured requirement or capacity reservation.

AWS documents **at least 3,500 PUT/COPY/POST/DELETE or 5,500 GET/HEAD requests per second per partitioned prefix** and scaling across prefixes. These are not fixed bucket-wide ceilings. Scaling is gradual and can produce temporary `503 Slow Down` responses. A slash-delimited prefix is a key grouping, not a customer-provisioned physical partition. In particular, choosing 256 names does not instantly provision 256 independent performance allocations. [S3 performance](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance.html), [prefix organization](https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-prefixes.html).

Hash distribution is a FRY proposal to spread concentrated arrivals predictably. AWS no longer requires randomized key names as a universal performance rule; logical and sequential names are supported. Date and tenant grouping alone would not deliberately spread a batch concentrated on today's date and one inbox. Avoid rotating the whole active stripe set every day without testing the scaling impact. [AWS naming guidance](https://aws.amazon.com/about-aws/whats-new/2018/07/amazon-s3-announces-increased-request-rate-performance/).

Organization ownership and authorization remain in FRY's metadata and access checks; opaque names and hash stripes are not security boundaries. Clients do not receive bucket-listing or general S3 credentials. A customer needing an independent IAM, encryption, or operational boundary can require a separate bucket or storage binding; this is distinct from throughput striping. This layout does not resolve the separate question of cross-organization physical-object sharing. IMAP enumeration, search, and retention selection use the database, not prefix LIST calls.

Capacity validation must count aggregate API operations, including multipart parts/control requests, repeated body/range fetches, HEAD, cleanup, migration, and retries. One message does not necessarily equal one PUT and one GET. Use bounded concurrency, retry backoff with jitter, per-organization fairness, and observable per-stripe request/latency/error rates. If the original cannot be safely persisted within the receipt deadline, return a temporary SMTP failure and leave IMAP collection checkpoints unchanged. Never acknowledge before the existing acceptance contract is met. SSE-KMS, if selected later, introduces separate KMS request quotas that prefix distribution does not bypass. [S3 design patterns](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance-design-patterns.html).

Benchmark abrupt cold and sustained traffic, concentrated inbox batches, migration alongside receipt, and cleanup alongside reads; do not infer peak capacity from the daily mean. Detailed evidence: [S3 key layout and throughput](../research/s3-key-layout-and-throughput.md).

## Go modules and process roles

The protocol-facing processes use the same core mailbox/persistence logic as workers and administration. They must not each implement a different deletion or permission policy.

1. **Receipt and persistence:** stream originals, retain envelope information, resolve authorized recipients, commit recoverable receipts, expose bodies under authorization, and determine cleanup eligibility.
2. **Mailbox state:** folder membership, persistent IMAP identifiers, flags, changes, search metadata, and concurrent-session behavior. Body bytes stay in object storage; mailbox operations normally change metadata.
3. **Mail protocols:** SMTP receiver/submission and IMAP frontend translate protocol requests into core operations. A protocol library does not own the retained-original lifecycle.
4. **Work execution:** bounded batches for mailbox materialization, source collection, outbound handoff, cleanup, and migration. Use fair per-organization scheduling and explicit concurrency/admission limits; an unbounded queue is not a capacity strategy.
5. **Control and administration:** organizations, domains, addresses, distribution aliases/catch-alls, credentials, sender authorization, relay settings, roles, audited recovery, and operational status.

Propose durable PostgreSQL work tables with short claims and expiring leases initially, not a mandatory separate broker. Receipt and pending work can commit together. External I/O happens outside queue-lock transactions. If queue churn or fan-out requires a broker, publish from committed work/outbox state and retain idempotent processing; the broker must not become an uncoordinated second acceptance ledger. [PostgreSQL queue-like row locking](https://www.postgresql.org/docs/18/sql-select.html#SQL-FOR-UPDATE-SHARE).

## Incoming SMTP: what makes a message accepted

1. Resolve configured recipient addresses, distribution aliases, and optional catch-all recipients. Reject unknown recipients when no catch-all exists. Preserve the original envelope separately from message headers and scope its exposure to the receiving organization/inbox.
2. Stream the full original into S3 under a generated receipt identity, with bounded buffers, size/admission limits, and integrity metadata. Do not retain a complete potentially 200 MB message in every worker's memory.
3. Commit receipt metadata, resolved destination set, ownership references, receipt timestamp, and recoverable pending work in a database transaction under the selected durability configuration.
4. **Only after the object is durably stored and the database commit succeeds, acknowledge final SMTP acceptance.** Receipt-based recovery retention starts here. Object storage and PostgreSQL are not one distributed transaction: interrupted writes can leave unreferenced objects; ambiguous commits need reconciliation using stable receipt identity.
5. Workers materialize one membership per destination inbox, in bounded batches. Several aliases resolving to the same inbox within this submission still produce one copy with relevant recipient metadata. A unique receipt/inbox work identity prevents worker retries from duplicating mailbox entries. No optional filtering or application routing is required on this path.

The object-first ordering and transaction pattern are a design proposal derived from the [persistence research](../research/portable-storage-and-imap.md), not a built-in S3/RDS transaction. If the SMTP success reply is lost after commit, a sender may retry and create another receipt. Do not deduplicate unrelated deliveries solely by Message-ID or claim exactly-once SMTP.

A message body can back multiple mailbox memberships without duplicating its bytes. Cross-organization physical-object sharing remains an isolation/design decision; never expose another organization's envelope, memberships, or retention state through a shared reference.

## IMAP access and the hot-inbox constraint

Default clients using the same mailbox see the same folder/flag/deletion state. The NLB distributes connections to IMAP frontends; after authentication, a frontend locates the active owner for that inbox/account and forwards the session internally. A proposed ownership lease includes a fencing epoch stored in the authoritative database. Mutating transactions must validate ownership **atomically with the mutation**, not check once and write later. Old owners must cease serving authoritative state after losing the lease; takeover may disconnect clients and require reconnection.

Persist mailbox UIDs, UIDVALIDITY, memberships, flags, and sufficient change history to rebuild sessions. Keep large uploads, object fetches, MIME parsing, and bulk ingestion out of the mailbox serialization path. The owner coordinates the protocol's ordered state changes; it must not sequentially process entire email bodies. Search indexes cover the required headers, dates, and flags; index choice and query performance need retained-data-scale testing.

This remains the highest-risk part. One inbox can receive roughly 500,000 messages, so distributing different inboxes to different pods does not solve the hot-inbox case. RDS table partitioning is not write sharding. Before production, benchmark both a concentrated mailbox and aggregate traffic, then determine owner design, batch sizes, index layout, and whether database shards are required.

**Proton Gluon is not yet selected.** Its local state, default per-user SQLite, deletion/reconciliation behavior, and hot-mailbox costs need substantial evaluation for this model. Compare a Gluon adapter with an `emersion/go-imap` backend spike against actual client behavior. Go SMTP/MIME libraries are protocol building blocks, not the distributed persistence architecture. See [Gluon findings](../research/hot-inbox-feasibility.md) and [adaptation risks](../research/portable-storage-and-imap.md).

## Other first-milestone paths

**External IMAP collection:** schedule each connection's configured folders and destination mapping; read without deliberately changing source flags; write through the receipt/persistence path. Commit checkpoint and receipt association together or with an equivalent idempotent recovery protocol. Track source connection, folder identity, UIDVALIDITY, and UID; source renames/reset/moves still need explicit recovery and duplicate policy. Later source reads/moves/deletions do not mirror into FRY. Source copies stay by default; opt-in cleanup waits for the agreed safe-storage condition and is itself retryable work.

**Outbound SMTP:** authenticate and enforce approved sender domains/addresses, persist bytes and outbound queue state before accepting. Dispatch to the installation default or organization-specific MTA using 587/465. Store progress per recipient when relay acceptance is partial. Retry transient failures up to the configurable deadline; expiry triggers sender notification and an administrator-visible failure. Never equate relay acceptance with final recipient delivery. A lost relay reply can cause duplicate handoff on retry. Changes to relay configuration, early permanent failures, and failure-notice mechanics still need definition.

**Deletion and recovery:** normal IMAP deletion/expunge changes mailbox references. Recovery expiry cannot delete bytes still referenced by a live mailbox. Once all live references, active work, and retention/grace requirements end, transactionally mark an object eligible for deletion and prevent concurrent resurrection; an idempotent worker deletes the object and completes cleanup. Do not use an unconditional bucket lifecycle rule as the authority for visible-mail expiry. Orphan cleanup must also account for in-flight or ambiguous receipt commits. A client deleting an old message does not restart retention.

**Migration:** background import preserves history, folders, dates, and flags. Pause affected clients for final reconciliation and switching; restore reception/access within 30 minutes. Coordinated client checkpoint reset is permitted, and client resynchronization may continue afterward. Validate counts, bytes, state, retry behavior, and rollback before retiring old infrastructure; no source decommissioning is currently authorized.

## One availability zone fails

Healthy NLB/ALB targets and surviving EKS replicas handle new connections. Sessions attached to failed pods reconnect; ownership transfers only through the fencing protocol. If the database writer fails, reconnect through the managed RDS endpoint after failover and reclaim expired work safely. Retain all acceptance state in S3 and the configured durable database path, not pod disks.

S3 Standard and a synchronous RDS standby supply useful storage primitives. They do **not** prove FRY's full no-accepted-mail-loss / five-minute recovery target. RDS describes typical failovers around 60–120 seconds, not a fixed upper bound for the whole application. Test DNS/client reconnect, readiness, leases, queue recovery, backlog, and surviving capacity together. [RDS failover](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Concepts.MultiAZ.Failover.html), [proposal review](../research/architecture-proposal-review.md).

The agreed reduced-protection policy allows continued acceptance with alerts once safe ownership and surviving durable storage are established. Implementation must establish how the chosen managed database exposes replication health and when this state applies. **An unavailable authoritative database still prevents acceptance**, even if S3 is reachable; do not fabricate success while the recoverable receipt ledger is unavailable. No whole-region-failure guarantee is claimed for this installation.

## Later capabilities and costing implications

Filtering, quarantine review, ZIP inspection, application routing, independent application views, and application-delivery webhooks/acknowledgments remain later milestones. The durable receipt/work model creates places to add them, without making them prerequisites for ordinary mail. Multi-region synchronous acceptance requires a separate storage/coordination design; enabling asynchronous replication later is not sufficient by itself.

The [cost estimate](../research/sao-paulo-cost-estimate.md) priced one database pair, six workers, S3, one NLB, and fixed NAT/IPv4 costs. This proposal explicitly adds a web ALB and supporting secret/telemetry services; those were not included in the subtotal. Additional database shards, broker infrastructure if needed, burst capacity, cross-AZ traffic, and historical mailbox data can materially change the estimate. No fixed node or database count should be treated as validated production sizing.

Before turning this into implementation work, validate the acceptance failure matrix, IMAP library/ownership behavior, hot-inbox capacity at retained scale, and full AZ recovery. This proposal is reviewable architecture, not implementation approval or a proven service-level guarantee.
