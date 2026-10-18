# Review of the proposed regional architecture

Checked 2026-10-17. This is a feasibility review, not an architecture decision. Proposed components are EKS across three AZs, NLB/ALB entry points, S3 Standard originals, one PostgreSQL writer with a synchronous RDS Multi-AZ standby, SQL-backed work, and coordinated IMAP owners. Six worker nodes are a cost illustration, not validated sizing.

## Verified foundations

RDS Multi-AZ DB instances synchronously replicate to a standby in another AZ. That standby cannot serve read traffic; this deployment provides availability, not writer scaling. AWS describes failover as **typically 60–120 seconds**, potentially longer with large transactions/recovery, and requires reconnecting after its DNS change. Neither the typical time nor synchronous replication proves FRY's end-to-end five-minute target. [RDS architecture](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Concepts.MultiAZSingleStandby.html), [failover behavior](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Concepts.MultiAZ.Failover.html).

S3 Standard stores data across at least three AZs and is designed to survive an entire AZ's loss. EKS places its control plane across three AZs within one region. Application metadata, pending work, worker capacity, and mailbox takeover still need independent validation. These regional components do not establish whole-region recovery. [S3 durability](https://docs.aws.amazon.com/AmazonS3/latest/userguide/DataDurability.html), [EKS architecture](https://docs.aws.amazon.com/eks/latest/userguide/eks-architecture.html).

PostgreSQL documents `SKIP LOCKED` for concurrent consumers of queue-like tables. It does not supply scheduling fairness, leases, retries, or exactly-once external effects. [PostgreSQL locking](https://www.postgresql.org/docs/18/sql-select.html#SQL-FOR-UPDATE-SHARE).

## Critical design checks

- **Acceptance and recovery:** S3-first, then an atomic receipt/ownership/work transaction, then final SMTP `250` is a plausible sequence. It needs reconciliation for orphan uploads and ambiguous commits. Cleanup must not race live ingestion. If the final response is lost, senders can retry despite durable acceptance; identifiers and downstream handling must tolerate duplicates. SMTP itself does not promise exactly-once delivery. [RFC 5321 §6.1](https://www.rfc-editor.org/rfc/rfc5321.html#section-6.1).
- **Capacity concentration:** one database writer serves receipt records, mailbox mutations, searches, queue churn, and cleanup. An active mailbox owner can concentrate a 500,000-message inbox batch on one process. Measure both bottlenecks, including retained data, lock/WAL pressure, reconnect storms, and surviving-AZ capacity. Additional frontend pods do not automatically increase either limit. See [hot-inbox findings](hot-inbox-feasibility.md).
- **Fencing:** an epoch check before a write is insufficient if ownership can change before that write commits. Persistent mutations must enforce ownership atomically; stale in-memory sessions/notifications also need shutdown or validation. Database fencing cannot retract an external SMTP handoff. Long network operations must not hold queue-claim transactions open.
- **Durable mailbox publication:** pending work must recreate membership/UID state after crashes without duplicating publication or reassigning established UIDs. Gluon adaptation remains unproven; routing connections to an owner does not solve its storage/session semantics.
- **Object lifetime:** reference-aware cleanup must protect live mailbox references, recovery copies, and pending work. Concurrent restore, publication, and deletion require coordination. Persist deletion work before discarding the metadata needed to locate objects; a blanket bucket-expiry rule must not erase live mail.

No benchmarks, failover tests, or implementation review were performed. Required proof is a complete AZ-loss exercise under representative retained-data and hot-inbox load, including reconnect and recovery of every accepted receipt.
