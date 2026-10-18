# Local hot-byte store: first implementation stage

Status: implemented local Linux prototype, tested on 2026-10-17 with Go 1.27.2/linux/amd64. It implements the first local-storage step of the [storage protocol](../design/storage-protocol.md#implementation-sequence). It does not implement FRY receipt acceptance or validate a deployment's durability/recovery guarantee.

## Implemented contract

The [`internal/hotstore`](../../internal/hotstore/) package owns a private existing directory, one append file, its experimental format, and its staging files. Its small interface is `Open`, `Append`, `Read`, `Inventory`, `Close`, plus the explicit `ReconcileTail` recovery operation. Callers supply byte limits and maximum concurrent appends. The future receiptstore module will combine this internal byte adapter with authoritative metadata and replication; callers must not assemble successful SMTP acceptance from a local locator alone.

- `Append(ctx, key, reader)` stages raw bytes with bounded buffers outside the append lock. Keys contain receipt and attempt identities; they are not email Message-ID values. A completed stage is copied under the append lock, and the data file is synced before returning a locator. The first file header and directory entry are also synced. Staging is never itself an acknowledged location.
- Admission reserves twice the maximum encoded message size per active append, covering the staged and appended copies. The budget includes current append-file length and all reservations. This conservative prototype policy can reject small writes or retries near capacity even if their eventual payload would fit. It is separate from advisory organization quotas. Filesystem metadata, unrelated users of the disk, and real free-space management are outside this budget; actual I/O failures still stop the write.
- A repeated key with identical size and SHA-256 returns the existing locator; different content returns a conflict. Separate keys retain separate records even if their bytes match. This is local write-attempt retry behavior, not cross-message/business deduplication or authorization.
- `Read` accepts a locator and payload offset/length and streams to a caller-owned writer. It validates record identity/header/footer and each touched frame before exposing that frame's requested bytes. Full reads additionally check SHA-256. A later checksum error can occur after earlier bytes have been delivered. Partial reads do not validate unread payload frames or the whole-message digest.
- `Inventory` reports complete local records, including writes whose replies were lost. It is not the list of accepted FRY receipts. The prototype index is in memory and startup scans all records; neither approach has been validated at retained-data scale.
- `Close` waits for ongoing calls. Context cancellation is checked between I/O calls; an arbitrary blocked Reader/Writer or filesystem call cannot be interrupted by checking a context. Protocol adapters must own cancellation/deadlines for their network streams.

Local I/O errors after append starts return `ErrUncertain`, with no locator, and block further appends until close/reconciliation/reopen. Existing verified records remain readable. Staging cleanup errors also stop admission rather than silently ignoring unaccounted disk use. Restart syncs all recovered complete records before a retry can receive a locator; readable page-cache bytes alone are not treated as proof of prior durability.

## Experimental format

All integers are little-endian. The file begins with a 64-byte header containing magic/version, a random file incarnation ID, the 64 KiB frame size, reserved zero bytes, and CRC32C. Each record contains:

1. A 96-byte header: magic/version, receipt ID, attempt ID, raw length, SHA-256, reserved zeros, and CRC32C.
2. Independent frames of at most 64 KiB, each preceded by payload length and frame index and followed by CRC32C covering frame metadata and payload.
3. A 64-byte footer binding encoded record length and header SHA-256, with reserved zeros and CRC32C.

The prototype rejects raw records above 256 MiB and accepts a separately configured lower maximum. The exercised 200 MiB stream is a test case, not a selected production SMTP size limit. No whole-record compression or MIME reserialization occurs. Integrity checks detect corruption; they are not authentication against an attacker who can rewrite files and checksums.

The initial file has a persistent random identity and a process-exclusive `flock`. Callers must provide a private local directory with a durable parent chain and must not replace the lock file or modify data concurrently. Process locking alone does not establish multi-node fencing or protect against arbitrary shared-storage writers. File rotation, member identity/placement, dedicated oversized files, and pack encoding remain later work; this adapter currently uses one append file.

## Conservative recovery

Startup validates headers, lengths, frame indices/checksums, whole-record digests, footers, and duplicate keys. Interior corruption returns an error and leaves the entire file unchanged, preserving later valid records for diagnosis/repair. An incomplete record at EOF returns `IncompleteTail` with the file identity, last valid boundary, and observed file length; startup does not truncate it.

`ReconcileTail` requires that observed report and the **complete authoritative list of still-required locators for this file**, with metadata publication fenced throughout recovery. It checks every supplied locator against the verified prefix and refuses truncation if any protected record is absent or damaged, or if the report is stale. Only an explicitly reconciled incomplete tail can be truncated and synced. The adapter cannot establish that the external locator list is complete or that publication is fenced: those are mandatory coordinator prerequisites for the next implementation stage, not inferred from an empty list. Tests supply parent-owned synthetic recovery evidence.

Orphan staging files are removed on successful startup under the exclusive directory lock. No successful locator ever points to staging, and complete append copies are checked/synced first. An incomplete/corrupt append file prevents that startup cleanup until reconciliation. This is not a generic deletion facility, reference-lifecycle GC, or evidence of 24-hour physical reclamation.

## Executed validation

The following checks passed locally:

```sh
go test ./... -count=1
go test -race -short ./...
go vet ./...
```

The initial suite and race checks used the host's default `/tmp` (tmpfs). The complete suite, including the 200 MiB case and subprocess exits, was then rerun with `TMPDIR` pointing to a temporary directory on the workspace's disk-backed Btrfs filesystem and passed. Temporary test data was removed afterward. For equivalent disk-backed testing, set `TMPDIR` to a private scratch directory on the filesystem being evaluated; a default temporary directory may be memory-backed.

Tests exercise:

- Empty records, exact frame boundaries, unaligned limits, cross-frame ranges, raw-byte preservation, and restart.
- A synthetic 200 MiB streaming append, full digest verification, a small range read, and reopening. The largest requested source read is 65,536 bytes; no whole-body allocation is needed by the adapter. This does not constitute a peak-memory or throughput benchmark.
- Duplicate-key retries, distinct-attempt isolation, mismatched locators, invalid ranges, canceled streams, broken/stalled readers, and short output writes.
- Concurrent appends and same-attempt retries under the Go race detector; blocked sources do not hold the append lock. Concurrency/capacity exhaustion rejects work before reading it.
- Exclusive ownership, close/reopen, injected sync failure, and an injected error after a partial append. Error injection verifies state handling; it is not a real failing disk or an actually full filesystem.
- Corrupted file/record headers, payload frames, frame ordering, footers, oversized encoded lengths, and a modified payload with a recomputed frame CRC. Whole-record SHA-256 catches the latter on full read and restart.
- Incomplete record headers, payload frames, and footers; protected-locator and stale-report rejection; preservation of the valid prefix and later records on corruption.
- Abrupt child-process exit after staging, during append, before sync, and after sync. The child does not run deferred cleanup. The parent retains prior locators and verifies prior bytes plus safe retry behavior after reopening/reconciliation.

These subprocess tests exercise process termination while the host kernel/filesystem remain running. They do not simulate power loss, loss of page cache, torn sector writes, network partitions, EBS failure, or whole-AZ loss. The complete pre-sync record can remain readable after process exit; startup deliberately syncs it before reusing its locator. Hardware durability remains dependent on the filesystem/device honoring successful sync calls.

## Next gate

Progress update: the first [PostgreSQL/two-local-member receipt implementation](replicated-receipts.md) now exercises the attempt/receipt guards, streaming replication, and cancellation described below. It does not yet supply full startup recovery coordination, remote members, or physical reclamation.

Implement the authoritative PostgreSQL attempt/receipt/target state and local-member cancellation fencing, then stream to two members and commit receipt state only after the required durable placements exist. Test ambiguous database commits, stale operations, abort/cleanup races, and replica loss. S3 publication, compaction, lifecycle read pins, repair, fairness, rolling operation, protocol compatibility, and the five-minute/24-hour guarantees remain unimplemented and unvalidated. See the [system failure matrix](storage-failure-validation.md).
