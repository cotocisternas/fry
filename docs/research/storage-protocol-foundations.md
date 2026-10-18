# Foundations for FRY's storage protocol

Verified 2026-10-17. Scope: PostgreSQL-authoritative acceptance, two hot copies, asynchronous S3 packs, and active-storage reclamation. This records platform facts and resulting design obligations; it does not demonstrate implementation correctness or the five-minute recovery target.

## Database authority and fencing

At PostgreSQL READ COMMITTED, ordinary reads use a statement-start snapshot. An updating/locking statement waits for concurrent modifications and re-evaluates its target row against the updated version. `SELECT FOR UPDATE` returns that updated row. Complex predicates spanning different rows can still observe inconsistent combinations. [Transaction isolation](https://www.postgresql.org/docs/current/transaction-iso.html)

FRY implication: publishing, reclaiming, acquiring/renewing read pins, and changing an ingest/work generation must serialize on the same durable guard rows. Acquire those rows in a consistent order, then recheck state and generation in the transaction that changes authority. A prior SELECT, process-local lease, or generation check before a later independent write is insufficient. Locking a missing row does not reserve it: persist the attempt/pack identity first, using uniqueness constraints. Row locks last to transaction end; workers must not hold transactions across unbounded network transfers. [Explicit locking](https://www.postgresql.org/docs/current/explicit-locking.html)

`now()` and `CURRENT_TIMESTAMP` report transaction start; `clock_timestamp()` reports actual current time. Evaluate lease deadlines after acquiring locks using current database time, and distinguish time-based eligibility from generation-based fencing. Database locks fence database decisions, not an already-started external upload. [Date/time functions](https://www.postgresql.org/docs/current/functions-datetime.html)

## Durable local bytes

Linux `fsync` flushes file data and associated metadata, but does not necessarily persist the directory entry; creation/rename durability needs appropriate parent-directory synchronization. `fdatasync` includes metadata necessary for correct later retrieval. Check errors and only acknowledge the completed durability operation, under tested filesystem/device semantics. [Linux fsync](https://man7.org/linux/man-pages/man2/fsync.2.html)

Protocol consequence: each record needs bounded framing, identity, length, integrity checks, and a recoverable committed end. A failed append cannot truncate later valid records. Byte GC must first make an attempt permanently unpublishable in PostgreSQL, then coordinate local in-flight appends; absence of an accepted row is not sufficient evidence of garbage.

Unlinking the last filename does not release a file until all open descriptors close. Cleanup evidence must account for reader/packer/repair handles, not merely SQL pin expiry and successful unlink. [Linux unlink](https://man7.org/linux/man-pages/man2/unlink.2.html)

## S3 publication and integrity

`If-None-Match: *` supports conditional creation with PutObject or CompleteMultipartUpload. Existing current objects cause a precondition failure; concurrent deletion can produce conflict responses. In-progress multipart uploads are not existing completed objects for this check. Use unique pack-attempt keys, durable upload identities, and reconciled retry outcomes. Conditional creation does not atomically publish the PostgreSQL index or prevent recreation after deletion. [Conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html)

S3 validates supplied upload checksums. Multipart ETags and SSE-KMS ETags are not whole-object MD5 digests; full-object and composite checksums differ. Store the algorithm and checksum type alongside the value. A HEAD response is metadata, not rereading/verifying every byte; use a defined upload-verification contract, then validate record checksums during reads. [Upload integrity](https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html), [HeadObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html)

GET supports one contiguous byte range per request. Independent record framing/compression must permit verifying the requested message without restoring the entire pack. Readers need protection against retiring their selected location until their bounded operation finishes or safely fails/retries. [GetObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html)

## Deletion and recovery constraints

In a versioned bucket, ordinary DELETE adds a delete marker; prior object bytes remain. Permanent removal requires deleting the relevant versions. Track version IDs if enabled, include abandoned multipart parts and staging copies, and avoid claiming a missing current object proves purge. [Version deletion](https://docs.aws.amazon.com/AmazonS3/latest/userguide/DeletingObjectVersions.html)

Lifecycle expiration is asynchronous and can lag eligibility. It is not sufficient evidence of a 24-hour active-storage deadline. Use scheduled, verified cleanup and retain tombstones until late producers cannot resurrect bytes. Offline hot volumes cannot be physically cleaned while inaccessible: report overdue debt and prevent serving from them until reconciliation. Backup erasure remains separately unspecified. [Lifecycle expiration](https://docs.aws.amazon.com/AmazonS3/latest/userguide/lifecycle-expire-general-considerations.html)

RDS Multi-AZ DB instances synchronously replicate to a standby in another AZ. Typical failover is 60–120 seconds, with longer recovery possible; clients must reconnect. FRY's no-loss claim depends on acknowledged durable database commits, correct hot-byte placement, and the actual failure profile. Neither typical failover nor the service topology proves end-to-end recovery within five minutes. Test ambiguous commits, stale connections, and degraded acceptance explicitly. [Multi-AZ replication](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Concepts.MultiAZSingleStandby.html), [failover behavior](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Concepts.MultiAZ.Failover.html)

## Candidate protection against late PUT completion

AWS says concurrent same-key conditional creates permit the first completion and reject later writes. Inference: after irrevocable database retirement, a privileged GC worker can replace the payload with a permanent zero-byte object using `If-Match`, or create that tombstone using `If-None-Match` if absent, retrying conflicts. A delayed create-only uploader cannot overwrite the tombstone. This is an ordinary object, not an S3 delete marker. [Conditional-write behavior](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html)

This candidate requires a never-versioned bucket, unique preregistered keys, no marker deletion/lifecycle expiry, and worker permissions restricted to conditional creation; protect against copy/overwrite/delete bypasses. AWS supports bucket-policy enforcement of conditional headers. Do not copy email-bearing metadata into markers. Permanent markers accumulate keys and operational cost. The proposed initial single-PUT-only scope avoids unfinished multipart parts; future multipart support needs separate cleanup. This reasoning is not a concurrency-test result. [Policy enforcement](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes-enforce.html)
