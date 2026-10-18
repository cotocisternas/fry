# Two-site and three-site topology: durability and cost

Research date: 2026-10-17. Site count, geography, products, and deployment topology remain undecided. This note compares mechanisms and cost drivers using primary sources; it is not a vendor recommendation or capacity assessment.

FRY's resilient multi-site deployment profile requires no loss of accepted email after an entire datacenter/cloud-region failure, with new receipt and consumer access restored within five minutes. Both self-hosted and managed deployments are required. Independent single-site and single-region installations are also permitted with explicitly narrower guarantees; their exact targets remain open. The comparisons below concern the resilient profile. Reported volume is tens of millions/day, including 500,000-message single-inbox batches of unknown duration. Usually below 1 MB is a size category, not a measured mean. Retention is days to months.

## What acceptance must protect

**Design implication:** a receipt cannot depend solely on the site acknowledging it. Original bytes plus recoverable ownership/envelope metadata and pending work must survive that site's loss. Protecting the database while leaving blobs regional, or vice versa, is insufficient. An independently replicated ingest record could carry recovery state instead of requiring every derived index to be synchronous; that is an unevaluated option.

PostgreSQL synchronous replication can wait for standby WAL durability; it adds network round-trip latency and depends on standby placement/configuration. Default asynchronous replication can lose committed transactions during failover. These are database guarantees, not protection for separate blobs or automatic application recovery. [PostgreSQL replication](https://www.postgresql.org/docs/18/warm-standby.html#SYNCHRONOUS-REPLICATION).

S3 cross-region replication explicitly copies asynchronously. Therefore enabling CRR after a regional PUT does not establish remote durability before FRY acknowledges receipt. Region-spanning routing also cannot recover bytes never copied. [S3 replication](https://docs.aws.amazon.com/AmazonS3/latest/userguide/replication.html).

## Two data sites

Confirming durable receipt in both sites can protect already accepted mail against either site's loss. Election and fencing remain separate: a broken link does not tell either side whether the other failed or is still serving clients.

For majority consensus, etcd documents that two voting members require both votes; three require two. Placing three voters across only two sites leaves one site's loss able to remove the majority. This is a placement constraint, not a claim that every two-site system uses etcd. [etcd quorum and cross-region tradeoffs](https://etcd.io/docs/v3.6/faq/).

**After one data site fails:** the surviving copy preserves old mail, but new writes have only one available data location. Continuing acceptance with the unchanged invariant that every new receipt survives a site loss requires a second durable destination. Otherwise FRY must wait for restoration or explicitly adopt a degraded protection policy. The user permits temporary acceptance with weaker protection and operator alerts. New mail accepted in that mode may not survive a further site loss before replication is restored. Activation is automatic with operator alerts once safe ownership is established. The user permits continued acceptance until replication recovers, without an elapsed-time cutoff, while surviving storage remains healthy and ownership remains safe, with ongoing alerts. Storage-health/capacity thresholds and repair remain undecided; this policy does not solve election or fencing.

## Two data sites plus a third arbitration site

An independent third coordination location can support majority-based ownership decisions. It does not replace fencing that prevents an old owner from writing; Patroni's watchdog documentation explains why losing a leadership lock alone is insufficient in some process/OS failure cases. Use established mechanisms, not a new consensus algorithm. [Patroni watchdog](https://patroni.readthedocs.io/en/latest/watchdog.html).

Here, “witness” describes a role without full email storage, not a special etcd node type: an ordinary etcd voter stores its coordination data. Losing a data site still leaves only one location for email bytes. The witness solves neither that replication gap nor full recovery from another data loss. If it durably stores enough message data to close the gap, it is data-bearing and must be costed accordingly.

## Three data-bearing sites

With correctly placed coordination and storage, one lost site can leave two connected data locations for new durable receipts. etcd's majority partition continues while the minority cannot write; it pauses writes during leader election. That supports the coordination concept, not a complete FRY availability guarantee. [etcd failure behavior](https://etcd.io/docs/v3.6/op-guide/failures/).

The acknowledgment policy still matters: waiting for all three copies differs from waiting for a safely coordinated quorum of two and filling the third later. After a failure, older messages may initially have only one surviving copy under the latter policy. Repair lag and consecutive/overlapping failures need explicit treatment. Three sites do not guarantee five-minute recovery, enough surviving capacity, safe failback, or protection from correlated dependencies.

## Cost model without prices

Define `N` messages/day, measured mean complete-message size `S` bytes, retention `D` days, and `R` full logical copies:

- Original bytes/day: `B = N × S`; retained originals: `L = N × S × D`; replicated retained bytes: `R × L`.
- For source-to-replica copying once per object, logical replication payload/day: approximately `(R − 1) × B`. Add metadata/WAL, retries, repair, protocol overhead, and consumer traffic.
- With one object/message/copy, baseline object creations/day: `R × N`. Add reads, integrity checks, listings, versioning, extracted artifacts, and retries. Batching/packing changes this model and its recovery tradeoffs.

**Hypothetical illustration only:** 10 million/day × 100 KB mean × 30 days equals 1 TB/day and 30 TB retained, using decimal units. Two full copies mean 60 TB and roughly 1 TB/day replication payload; three mean 90 TB and roughly 2 TB/day. Baseline creations are 20 versus 30 million/day. These are not FRY's measured values or billed totals.

The user subsequently clarified that default shared-mailbox messages remain until client deletion. The formula above estimates only retention-window storage; live mail older than that window must also be counted, without double-counting any shared underlying bytes. It is not a bound on total storage.

Add independent application-view memberships/flags/folders, indexes, delivery attempts, audit history, logs/backups, retention grace, and temporary backlog. A third full copy raises this simplified raw-storage component by 50% over two, not necessarily total cost by 50%. A witness avoids full raw-mail storage but retains coordination/operations costs and the durability limitation above.

Billable transfer differs from logical bytes: direction, route, provider, region, and network products determine charges. AWS, for example, charges destination storage, replication PUTs, and applicable inter-region transfer separately. Self-hosting instead needs disk redundancy/headroom, WAN capacity, hardware replacement, and operational staffing. [S3 pricing dimensions](https://aws.amazon.com/s3/pricing/).

## Decision evidence still needed

Confirm allowed geography, failure independence, degraded-operation policy, inter-site latency/bandwidth, measured burst/mean size, retention and view counts. Cost warm capacity sufficient after one site fails, including scanners, IMAP ownership/state recovery, and backlog drain. Test detection, fencing, promotion, traffic rerouting, credentials/config availability, and client reconnect together against five minutes. No topology, monthly quote, or demonstrated recovery claim follows from this note.
