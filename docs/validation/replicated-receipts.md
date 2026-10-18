# PostgreSQL receipt authority and two-member ingestion

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Status: second local implementation stage, tested on 2026-10-17 with Go 1.27.2/linux/amd64, pgx v5.11.0, and PostgreSQL 18.6. It follows the [storage protocol](../design/storage-protocol.md) and builds on the [local byte store](local-hotstore.md). The members run in one process against separate local files. This is not remote authenticated replication, an HA database deployment, or evidence of an availability-zone recovery guarantee.

Subsequent progress: [storage lifecycle](storage-lifecycle.md) adds read pins, references, compaction, local reclamation and scratch recovery. Limitations below describe this stage's original scope.

This document records the original local stage. Subsequent [lifecycle](storage-lifecycle.md), [SMTP](smtp-reception.md), [IMAP](mailbox-imap.md), and [authenticated process transport](replica-transport.md) stages supersede its historical “not implemented” statements. The transport evidence retains the current recovery limitations.

## What is implemented

[`internal/receiptstore`](../../internal/receiptstore/) owns the acceptance sequence. `OpenLocalMember` opens and exclusively owns a hot-byte file and its cancellation registry together. A second member cannot acquire the same directory. `New` requires two distinct member identities, file incarnations, and configured failure-domain labels. These labels are trusted configuration; tests on one host do not establish physical independence.

`Accept` requires an attempt ID, an already-authorized and resolved envelope, and a closeable stream. It takes ownership of the stream; closing it must unblock reading. The storage module does not implement SMTP address resolution, authenticate the caller, or prove that an inbox belongs to the supplied organization. The eventual ingress layer must establish those facts.

The ingestion sequence is:

1. In a database transaction, register the attempt, receipt identity, destination/envelope snapshot, lease/generation, protection-profile epoch, and both member/file targets. No body is dispatched before this commits. Duplicate destination inboxes and recipient metadata are normalized within the request.
2. Feed the same source into two member pipes using a 64 KiB coordinator buffer. Each member applies its own bounded staging/disk admission and fsync contract. Backpressure from either member bounds the pipeline; failure cancels both pipes and closes the source.
3. Members register active operations under local locks and check the authoritative attempt/target generation before starting. They persist returned durable locators only while the attempt is still live under the corresponding database guards. Lease renewal belongs to the coordinator and stops acceptance on failure.
4. The final transaction locks the profile and attempt, checks generation/deadline/placement, and requires two agreeing byte lengths and SHA-256 digests. It inserts the receipt, deduplicated destinations, pending-mailbox references, a receipt-based recovery reference, and mailbox-publication work together. A supplied source-message identity is marked accepted in the same transaction. The lease is checked again immediately before commit after the publication work.
5. Return a receipt only after commit succeeds. The module records an acceptance timestamp inside that publication transaction; it is not an externally measured exact WAL flush timestamp. No SMTP endpoint is implemented yet.

The first schema is embedded in [`schema.sql`](../../db/schema/receiptstore.sql). `Migrate` creates it transactionally in a fresh schema and deliberately fails if those tables already exist. This is not a versioned production migration/upgrade system. Database and member lifetimes remain the caller's responsibility; stop/cancel operations before closing dependencies.

## Retries, source progress, and cancellation

An unexpired preparing attempt rejects overlapping `Accept` calls. Once its lease expires, the same immutable request can claim a higher generation against the same registered members; changed member incarnations require explicit recovery, not silent target replacement. Delayed reports and publication from an older generation are rejected. Normal protection currently requires two configured domains; profile changes invalidate in-flight claims. Automatic reduced-protection acceptance remains unimplemented.

After an uncertain commit, `Status(organization, attempt)` resolves whether acceptance happened. Retrying an accepted attempt with the same normalized metadata returns the original receipt and timestamp without reading its body again. Callers must reuse attempt IDs only for the same operation. Separate attempt IDs are not deduplicated by Message-ID or body digest.

For collection, the optional source identity consists of connection, folder, UIDVALIDITY and UID. A preparing attempt reserves that identity; another attempt gets a source conflict and must resolve the original attempt. Acceptance marks it complete atomically with the receipt. Abort releases an unaccepted source reservation. This is a per-message association, not an advancing highest-UID cursor: accepting UID 100 does not skip uncollected UID 99. Folder rename/reset/move semantics and the IMAP collector remain future work.

`Abort` serializes with publication on the attempt guard. It cannot abort an accepted receipt. It marks a preparing attempt terminally aborted and records one cleanup obligation per registered target before asking the members to cancel active streams. Cancellation waits for active operations to finish before recording `cancel_confirmed`. Queued operations and restarted members check the terminal database state and cannot revive it. Unconfirmed cancellation remains outstanding if a call fails or times out.

`cancel_confirmed` does **not** mean `bytes_removed`: physical deletion is not implemented, and cleanup records retain `bytes_removed=false`. Reconciliation must eventually account for unaccepted byte records and staging/append inventory, including writes whose final locator report was rejected. There is no automatic attempt reaper or cleanup worker yet.

## Reads and durability boundary

`Read` requires an accepted receipt in the supplied organization scope. It validates the stored locator through the hot-byte adapter. It may fall back to another local member, or an attached [published-pack adapter](packed-objects.md), only before emitting any bytes; an error after partial output requires the caller to restart its download. It does not concatenate duplicate prefixes from multiple copies. Read authorization beyond internal organization scoping, lifecycle pins, expiration, and automatic repair remain unimplemented.

Receipt transactions explicitly set `synchronous_commit=on`, including when the supplied connection's default is asynchronous. Startup requires `fsync` and `full_page_writes` to be enabled. This exercises local PostgreSQL durability; synchronous standbys and a surviving metadata quorum are separate deployment requirements. PostgreSQL documents the relationship between commit settings and configured synchronous standbys in its [WAL settings](https://www.postgresql.org/docs/current/runtime-config-wal.html).

Profile guards use shared row locks so independent receipts need not serialize on one exclusive profile lock; profile updates still conflict. Attempt guards serialize renewals, takeover, publication, and abort. Deadline checks use the database wall clock after locks are obtained. Disk/stream I/O occurs outside these transactions. See PostgreSQL's [row-lock semantics](https://www.postgresql.org/docs/current/explicit-locking.html) and the [pgx pool interface](https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool).

## Reproduce the tests

From the repository root, on Linux with Go 1.25 or newer and Docker:

```sh
./scripts/tests/receipts.sh
./scripts/tests/receipts.sh -race -short
go vet ./...
```

The runner starts a disposable PostgreSQL container bound to a random loopback port, uses a pinned image digest, creates isolated test schemas, and removes the container and its test storage afterward. Member/test files use a temporary directory on the workspace filesystem (Btrfs in the executed runs). Test credentials are disposable fixture values; no AWS account or production database is used. The full run includes the earlier 200 MiB local-store test; the replicated streaming test uses roughly 2 MB per receipt across four concurrent receipts, not a measured production workload.

Integration tests skip unless `FRY_TEST_DATABASE_URL` is set. For an independently managed **disposable test database**, it can be supplied directly to `go test`; tests create/drop randomly named schemas and need permission to terminate their own test backend for the disconnection test. The script supplies this environment itself. Running `go test ./...` without it is not evidence that PostgreSQL integration passed.

Both runner commands and `go vet ./...` passed. The integration suite exercises:

- Target registration before byte dispatch; no receipt with only one durable copy; rejection of disagreeing replica content.
- Atomic receipt/destination/reference/work/source progress, including duplicate destination aliases and out-of-order source UIDs.
- Member failure, retained cleanup obligations, and terminal abort without falsely recording physical removal.
- Durable copies with no receipt commit, lease takeover, byte-attempt reuse, and unchanged receipt identity after retry.
- A committed transaction followed by an injected lost-reply error, resolved through status and retry without duplicate work.
- Actual PostgreSQL backend termination before the final commit, with no partial publication or successful acknowledgment.
- Stale generations, protection-profile changes, renewal during a blocked source, expiry after lock contention, and expiry during publication.
- Concurrent abort/publication, cancellation of active streams, rejection of late/queued reports, and a reopened member's rejection of an aborted attempt.
- Reopening both byte stores and the coordinator, organization-scoped lookup, and reading from the surviving local member when the first is closed.
- Four concurrent multi-frame receipts and race-detector checks; no throughput or latency claim follows from this test.
- Rejection of duplicate member ownership and same-domain placement; explicit synchronous commit despite an asynchronous session default.

The lost-reply test injects an error after a real successful commit; it does not drop a TCP packet at an indeterminate point. Backend termination is a real connection failure before commit, not PostgreSQL failover or a whole-server outage. Member failure closes a local file store, not a datacenter. These distinctions remain part of the evidence boundary.

## Remaining work

Remote member transport/authentication and ownership fencing, authoritative topology/health and automatic degraded mode, PostgreSQL replication/failover, startup recovery for partial member files coordinated with accepted-locator authority, orphan reaping and full hot-file inventory, real S3/IAM validation of the locally tested [packing/publication stage](packed-objects.md), compaction, reader pins, reference release/retention workers, repair, mailbox workers and protocols remain outstanding. The database schema and per-process index are experimental and need retained-data-scale benchmarks. No production capacity, five-minute recovery, or 24-hour cleanup guarantee has been demonstrated.
