# Packed-object publication and cold reads

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Status: third local implementation stage, exercised on 2026-10-17 using Go 1.27.2/linux/amd64, PostgreSQL 18.6, pgx v5.11.0, and AWS SDK for Go v2 S3 v1.114.3. Tests use real PostgreSQL and the actual SDK against a narrow local HTTP contract fixture. **No requests reached AWS.** This adds code and concurrency evidence for step 3 of the [storage protocol](../design/storage-protocol.md); actual S3 concurrency and IAM validation remain deployment gates.

Subsequent progress: [stage 4](storage-lifecycle.md) adds reader ownership, reference lifetime, published-pack compaction, hot reclamation and scratch recovery. The stage-3 limitations below describe its original evidence boundary; use that report for current lifecycle behavior.

## Implemented contract

[`Packer`](../../internal/receiptstore/packing_linux.go) selects up to 1,024 explicitly supplied accepted receipts from one organization with matching recovery-retention settings. It registers the job, unique object key, membership, and receipt manifest versions before creating or uploading bytes. Keys are `packs/v1/<two hex digits>/<organization>/<random pack ID>`, distributing the leading key prefix while retaining organization scope. There is no automatic size/age scheduler yet.

The builder streams verified originals into a private local hotstore image using the existing framed format. The captured image has a fixed length, even if its source later grows. The configured pack cap includes framing and cannot exceed 256 MiB; oversized selections fail before uploading. A larger configured cap can accommodate the tentative 200 MB message, but the integrated pack tests use smaller synthetic bodies. Each packer admits one build at a time with an explicit scratch budget. This is per-instance admission, not shared-volume capacity management. A failed scratch removal stops subsequent builds on that packer until operator reconciliation; crash-left scratch reconciliation remains pending.

Image length, SHA-256 and per-receipt locators are recorded before PUT. The S3 adapter uses a single-part create-only PUT with a supplied SHA-256 checksum, followed by HEAD with checksum mode enabled. Publication requires the expected length, checksum and a nonempty opaque ETag. ETag is never interpreted as a content digest. A successful HEAD can resolve an injected lost PUT reply. These request fields follow the [PutObject API](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html) and [HeadObject API](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html).

Publication locks receipts in stable order, then the pack. It checks membership versions, current live references, the absence of another published pack, the job epoch and its database-clock deadline. Reference liveness is queried again in a fresh statement after lock acquisition, avoiding an old statement snapshot after waiting for a receipt guard. The receipt locators and published pack state become visible in one synchronous transaction. Body upload alone does not expose a pack. Concurrent overlapping jobs can upload separate objects, but only one can publish a given receipt; the losing job stays tracked for retirement.

The lease bounds one build/upload attempt; there is no renewal or automatic takeover worker. `Pack` returns the registered ID when later work fails. `Status(organization, packID)` resolves publication, and `Retire` handles abandoned attempts. A new build uses a new key. The embedded schema still initializes only a fresh database schema; it is not an upgrade mechanism for retained production data.

## Reads and retirement

Both hot copies remain after publication. `Store.Read` tries hot members first and then an attached cold adapter only if no bytes have been emitted. A cold read binds every GET to the published ETag, checks file/record identity and framing, validates touched frame checksums, and checks the original SHA-256 for a complete message. As with hot reads, an error after partial output means the transfer failed; callers must not acknowledge a complete download.

The current cold path uses **four range GETs per nonempty read**: file header, record header, footer, and a contiguous span containing the requested payload frames. The count is independent of message frame count. Empty reads use three metadata ranges. This bounds request amplification but does not achieve the pricing model's illustrative one GET/message. At its dated rate and 600 million nonempty cold reads/month, four GETs would cost $1,344 rather than $336 for that line alone. Hot hits reduce requests; partial fetches and rereads increase them. No cache credit, latency or throughput has been measured, and pricing assumptions must be revisited before deployment.

`Retire` first revokes publication under the pack guard and commits `retiring`. The privileged object client then conditionally creates or replaces the key with a permanent zero-byte marker, verifies it, and records `retired`. A delayed create-only uploader cannot overwrite that marker. An interruption leaves a retryable job; there is no DeleteObject path. The conditional requests follow [S3 conditional-write semantics](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html).

**Published packs cannot be retired in this stage**, including after references expire. There are no reader pins, reference-release workers, hot-copy reclamation or compaction yet. Keeping all published bytes avoids prematurely reclaiming active readers but cannot satisfy bounded storage or the 24-hour physical-removal target. Unpublished object retirement is not receipt deletion.

The S3 constructor rejects enabled or suspended versioning and any configured bucket lifecycle rules. It accepts separate producer/read and privileged cleanup clients. It does not establish that their credentials differ or prove IAM policy correctness. Deployment must prevent unconditional producer overwrites, deletion of markers, unsafe versioning/lifecycle changes, and other writes that violate the object contract. The local fixture does not emulate or prove any of those controls.

## Executed checks

From the repository root, using Linux, Go and Docker:

```sh
./scripts/tests/receipts.sh
./scripts/tests/receipts.sh -race -short
go vet ./...
```

Both runner commands and `go vet ./...` passed. The runner creates and removes a disposable PostgreSQL container and isolated schemas. Ordinary `go test ./...` skips the PostgreSQL cases unless `FRY_TEST_DATABASE_URL` is set. No AWS account, live mailbox or production credentials are involved.

Coverage includes:

- SDK request checks for conditional creation/replacement, checksum-enabled HEAD, exact ETag-bound ranges, and rejection of unsafe bucket configuration or missing checksums.
- Publication of multiple receipts, full and cross-frame cold reads after closing both hot members, empty originals, and retained hot inventory.
- Fixed snapshot length after a later append; corrupt frame, footer, truncated image and recomputed-frame-checksum mutations. Whole-message SHA-256 detects altered payload even when its frame CRC is recomputed.
- Lost PUT response resolved through HEAD; a committed publication followed by an injected reply error resolved through status.
- Upload without publication, retirement before a delayed create, permanent marker preservation, and refusal to retire published data.
- Changed epoch, expired lease, changed receipt manifest, removed references, overlapping publishers, and reference removal while a publisher waits on a real PostgreSQL row lock.
- Organization scoping, duplicate/oversized selection rejection, and refusal to publish an unexpected uploaded body.

The HTTP fixture deliberately implements only the SDK request contract used here. It is not an S3 emulator, provider durability test or IAM verifier. Lost-reply tests inject an error after a successful operation; they do not reproduce an indeterminate network failure. PostgreSQL locks are real, but both members, the database container and HTTP fixture run on one host.

## Remaining gates

Run the same conditional-write/retirement races against an isolated real S3 bucket with the intended producer and cleanup roles, including denied overwrites/deletes and configuration drift. Add restart/orphan reconciliation, reader coordination, reference release, published-pack compaction, hot-file reclamation, bounded cleanup debt and size/age scheduling. Remote replication, metadata HA, capacity/fairness, six-hour upload outage recovery, timed AZ loss, and SMTP/IMAP compatibility remain separate requirements. No throughput, five-minute recovery or 24-hour cleanup guarantee is established by this stage.
