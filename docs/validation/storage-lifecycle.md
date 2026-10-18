# Reference lifetime, readers and active-storage reclamation

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Status: local implementation stage 4, dated 2026-10-17. Builds on [receipt acceptance](replicated-receipts.md) and [packed objects](packed-objects.md), using the same real PostgreSQL/local-file integration harness and SDK HTTP fixture. This report distinguishes implemented mechanisms from the deployment and load guarantees still outstanding.

## Reference lifetime

`AddReference` and `ReleaseReference` serialize on the receipt guard. Visible mailbox ownership is indefinite until released; delivery ownership may have an expiry. Acceptance alone creates the receipt-based recovery reference, which these operations cannot shorten or restart. Transfer pending-mailbox ownership by adding a stable mailbox reference before releasing the pending one. Released reference identities remain tombstoned, preventing a delayed add retry from reviving a completed owner.

`SweepExpiry` finds receipts with no current owners. The transition to `retiring` preserves an immutable eligibility timestamp and cleanup deadline. Natural expiry uses the final reference's actual expiry time, even when a sweep runs late. A release records its effective time without moving an earlier expiry forward. New references and new body reads cannot revive a retiring receipt. Receipt/attempt audit records remain after byte removal, so acceptance retries still resolve their original outcome.

The prototype configures a 24-hour cleanup deadline by default through `Options`; it does not yet implement the administrative organization-policy layer. Cleanup status reports remaining hot copies, pack/staging obligations, actual reader pins, current overdue debt, and whether completion missed the original deadline. A late completion does not erase that history.

## Readers own actual completion

`Open` checks organization scope, current references and location state under the receipt/container guards, then records a read pin and captures the selected cold generation. Its returned `ReadHandle` owns a bounded streaming pipe. `Close` cancels and waits for the underlying file/object read to return. `Read` is a convenience wrapper over the same path.

The current implementation uses a fixed maximum read lifetime (five minutes by default, configurable downward), rather than a renewable short lease. An unconsumed pipe is canceled at that limit. This bounds storage handle ownership even when a caller stops reading; arbitrary caller-owned `io.Writer.Write` calls can still block outside the storage reader. Legacy client timeout compatibility remains to be measured.

The owner removes its database pin only after the underlying read returns and closes its resources. **An expired pin is never treated as proof that a handle closed.** Database failure or an interrupted owner can leave a conservative unresolved pin. The local implementation does not yet provide distributed owner fencing or automatic reconciliation of orphaned read pins; these remain visible cleanup debt. A timeout must not silently erase them.

## Pack compaction

`Compact` snapshots a published pack's membership under receipt guards, selects still-needed originals, and registers a new attempt/key. Copying uses the same verified, pinned read path as callers. Publication rechecks versions, current locations and live references, then atomically switches surviving receipts and marks the old pack retiring. If a survivor expires during copying, publication fails and the source stays available for a new attempt. A wholly expired pack skips creating an empty replacement.

Existing readers retain their captured old generation. `Retire` refuses any pack with unresolved pins, including expired pins. Once those reads close, it replaces the old object body with the existing permanent empty marker protocol. Hot copies and abandoned building attempts remain independently accounted for; object retirement alone cannot finish receipt cleanup.

## Local hot-file replacement

`ReclaimHot` excludes reads, admission and writes for one local member while rewriting its current file. Unpacked live messages and pinned records are copied to a new generation in the same configured failure domain. Expired/unpinned messages are omitted. Published originals without active readers may leave hot storage entirely and use their verified cold location. This permits hot-file cleanup without S3 availability when live originals still need their local copies.

A durable rewrite job is registered before creating its destination directory. Copying verifies the old inventory against authoritative locators and checks the resulting size/digest. A second guarded transaction rechecks membership and publishes replacement locators plus the member's current generation. Only afterward does the member close and remove the old file, fsync its directory, and record physical removal. Preparing ingest targets and unknown local inventory block rewriting. Aborted targets need confirmed cancellation first.

The member root has its own exclusive file lock, separate from generation locks. Restart uses the database's committed generation: unpublished replacement files are removed, while published replacement files survive and predecessor cleanup resumes. An uncertain publication freezes the member until close/reopen reconciliation. Existing data, not an absence of a SQL reply, determines the recovered generation.

The configured member byte budget covers the old file plus the replacement builder's append/staging reservations. After predecessor removal, reopening restores the full single-file budget. Filesystem metadata and unrelated files are excluded, as in the underlying adapter. This is a **maintenance pause for a whole local member**, not online sealed-file rotation or a demonstrated high-throughput reclamation design. It can delay reception and reads using that member. No five-minute recovery or cleanup throughput claim follows.

## Pack scratch recovery and completion

A packer exclusively owns its private scratch root through a durable file incarnation and file lock. Each job records that owner before any scratch bytes are written. Normal completion closes all file handles, removes the job directory, fsyncs the scratch root and records `staging_closed`. Reopening the same scratch owner revokes unpublished jobs before deleting their leftovers; published objects stay published. Interrupted cleanup remains retryable and visible.

`FinishCleanup` marks a receipt `removed` only when both hot-target obligations, every associated pack/staging obligation, and every read pin are resolved. The operation retains the receipt's audit identity, original deadline and completion timestamp. Application-level removal is not secure hardware sanitization, deletion of infrastructure snapshots/backups, or a guarantee about inaccessible volumes.

## Validation

Run on Linux with Go and Docker:

```sh
./scripts/tests/receipts.sh
./scripts/tests/receipts.sh -race -short
go vet ./...
```

New integration cases cover indefinite visible ownership after recovery expiry; idempotent releases without renewed retention; rejection of reference resurrection; original deadlines after delayed sweeps; stalled-reader cancellation; compaction with a pinned old view; refusal to retire an expired unresolved pin; survivor expiry during upload; hot rewrites with S3 absent; cold reads after hot eviction; pending-ingest fencing; aborted-byte accounting; and refusal to report removal while another replica or scratch obligation remains.

Injected failures before/after hot-file publication are followed by member reopen and byte verification. Scratch recovery tests reconstruct the persisted pre-cleanup boundary and reopen the owner. These exercise real files and SQL but are not process-kill, device power-loss, distributed partition or AWS durability tests. Earlier hotstore subprocess-exit cases remain part of the suite. The test runner now waits for PostgreSQL's TCP listener, avoiding its temporary socket-only initialization server.

## Remaining work

Automatic fair work scheduling, sealed-file rotation, bounded rewrite batches at retained-data scale, automatic orphan read-owner fencing, replica repair, distributed member transport, database failover and real S3/IAM tests remain outstanding. A configured six-hour outage buffer and 24-hour deadline still need capacity and failure measurements. These storage mechanisms do not yet implement mail protocols, administration, migration or organization policy management.
