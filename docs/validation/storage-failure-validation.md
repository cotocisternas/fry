# FRY storage failure validation

Status: a bounded executable specification, local Go byte-store prototype, PostgreSQL/two-local-member receipt stage, packed-object publication/cold-read stage, and storage lifecycle stage have run locally; production storage has not been implemented or tested. This plan accompanies [storage-protocol.md](../design/storage-protocol.md). No AWS account, paid resources, message traffic, or production data were used. See [local byte-store evidence](local-hotstore.md), [PostgreSQL receipt evidence](replicated-receipts.md), and [packed-object evidence](packed-objects.md) for executed tests and their limitations.

## Evidence completed

Run `python3 docs/validation/storage_state_model.py`. The current model explores **1,456 reachable states and 3,882 transitions** for one receipt, two hot-copy AZs, one pack key, two ownership epochs, one reader pin, and at most one AZ loss. All specified safety invariants hold under its assumptions. [Machine-readable results](storage-state-results.json).

Reachable paths include a lost/delayed SMTP acknowledgment, database outage after commit, zone loss, explicit degraded acceptance, worker takeover, expiry with an active reader, and pack retirement. Six small two-record ordering checks also exercise membership snapshots and preservation of a surviving reference. This is a deliberately limited state space, not proof of arbitrary concurrency.

Three deliberately weakened rules produce concrete counterexamples:

- Without the attempt guard, cleanup can remove bytes before a stale acceptance commits.
- Without the epoch guard, a worker can publish after another worker takes ownership.
- Deleting a pack key instead of retaining an empty marker allows a delayed create-only PUT to recreate retired email bytes.

These mutation witnesses help show that the checks distinguish the intended protocol from specific unsafe variants. They do not test real PostgreSQL locks, file descriptors, hardware durability, S3 policies, or network timing. The model treats database transitions and S3 conditional operations as atomic and assumes truthful durable-write receipts. It does not validate the five-minute RTO, 24-hour cleanup liveness, IAM, multi-record transactions, hot-file scanning, capacity, or performance.

The [pricing model](../research/packed-storage-cost-model.py) separately runs arithmetic/invariant checks and reproduces the unchanged baseline. Those results concern cost arithmetic only.

## Implementation failure matrix

Each row is a required system integration/fault test; the evidence column distinguishes completed local checks from outstanding system behavior. Persist receipts and expected raw-byte digests outside the process under test, then compare recovered acceptance and data; do not rely only on logs claiming success.

| Injection | Required observation | Present evidence |
|---|---|---|
| Crash after registering attempt, before byte dispatch | No accepted mail; target obligations can be resolved safely | Real SQL target registration before dispatch and no premature visibility tested; coordinator process-kill coverage pending |
| Crash during staging/append/footer/fsync, or ENOSPC/fsync error | No durable acknowledgment for incomplete data; recover valid prefix without losing later accepted records | Local subprocess exits, incomplete-tail reconciliation, and injected append/sync errors pass; actual device failure, ENOSPC, power loss, and receipt integration pending |
| Coordinator dies after member fsync, before receipt commit | Stored bytes remain tracked by preregistered targets; abort/retry cannot create visibility accidentally | Injected failure after both local copies, real SQL takeover/retry and no duplicate byte records tested; process-kill variant pending |
| Database commit succeeds but connection/reply is lost | Status/retry returns same receipt; source checkpoint does not skip or double-publish membership | Injected reply error after real SQL commit resolves via status/retry; atomic per-source-message association tested; indeterminate network loss pending |
| Abort races stale byte completion or metadata commit | Aborted attempt cannot accept; all staging and appended copies remain tracked until removed | Real SQL abort/publication race and local active/late/queued/reopened-member cancellation tests pass; registered aborted-copy reclamation tested locally |
| One AZ is lost before/after acceptance | Every normal accepted receipt remains recoverable; reception and IMAP access restored within five minutes | Abstract single-loss preservation only; timed system test pending |
| Replica availability/profile changes during receipt | Commit rechecks profile and placement; explicit degraded mode is auditable | Same-domain/incarnation rejection, replica digest agreement, and changed profile epoch rejection tested; actual placement/authentication and degraded mode pending |
| DB writer unavailable or stale connection | No new acceptance without authoritative commit; reconnect and resolve ambiguous outcomes | Actual backend termination before commit prevents acknowledgment and partial publication; HA database outage/failover pending |
| Pack PUT completes but publication fails | Hot copies remain; uploader retry or orphan retirement is safe | Real SQL plus SDK/HTTP-fixture upload-without-publication, retained hot copies, and retirement tests pass; actual S3 test pending |
| Work lease expires during PUT | Old worker cannot publish; new worker resolves object identity safely | Real SQL deadline/epoch rejection and overlapping-publisher tests pass; actual S3 timing test pending |
| Retirement races old conditional PUT | Empty marker wins or replaces completed bytes; stale producer cannot overwrite it | Delayed SDK PUT loses to an empty marker in the HTTP fixture; real S3 concurrency/IAM test mandatory |
| Conditional-write policy absent, versioning enabled, or marker lifecycle configured | Startup/deployment validation rejects unsafe adapter configuration | SDK startup rejects enabled/suspended versioning and lifecycle rules in fixture; actual AWS policy/configuration tests pending |
| Read pin/renewal races reference expiry, compaction, or cleanup | No new pin on retired data; active reader closes before reclamation; no lease resurrection | Real SQL pins, bounded actual readers, expiry and compaction races pass; unresolved crashed pins conservatively block cleanup |
| Sparse expiry in mostly live packs | Preserve every live record; physically remove eligible bytes within configured deadline while healthy | Mixed-live/dead pack compaction, atomic locator publication, and pinned old-reader preservation pass locally against SDK/HTTP fixture; real S3 and deadline-under-load pending |
| Sparse expiry in hot append files during S3 outage | Same-AZ survivor copy and locator swap preserve placement; reclaim all expired copies | Local whole-member rewrite, survivor byte verification, pre/post-publication recovery, and preparing-attempt exclusion pass; online sealed rotation and load testing pending |
| Lost/paused process retains open descriptor after unlink | Do not report physical removal until handles close; track overdue debt | Actual local handles drain before removal; root lock fences local restart; expired unresolved pins remain cleanup debt. Remote owner fencing pending |
| Bad frame/identity/offset/interior record | Fail transfer, choose verified alternate, schedule repair; never truncate healthy later data | Local identity/range/frame/digest/footer checks and non-destructive corruption handling pass; alternate replicas and repair pending |
| Six-hour S3 outage at design byte rate | Preserve accepted mail; local budget includes two copies, normal occupancy and workspace; defer before unsafe exhaustion | Sizing formula only; load test pending |
| Object-store recovery while receipt continues | Upload/reclamation drain backlog without starving new receipt or reads | Throughput test pending |
| Organization exceeds advisory quota | Alert; continue within safe shared capacity; no forced deletion | Policy specified, integration pending |
| Inaccessible replica/object store past cleanup deadline | Explicit overdue outcome; never false erasure success; stale member cannot serve on return | Protocol limitation, not a deadline guarantee |

## Load and recovery measurements

Use measured traffic when available. Until then, run labeled synthetic cases at 100/250/500 KB, including a 200 MiB stream test; none defines a production distribution or final admission maximum. Exercise a 500,000-message single-inbox batch with several hypothetical arrival windows and concurrent IMAP activity. Capture SMTP final-response latency, receipt-to-mailbox delay, first/last body-byte latency, database transactions/WAL/lock waits, fsync latency, memory, queue ages, pack fill, disk occupancy, and network bytes by path.

At the 20M/day, 250 KB illustration, average intake is about 232 messages/s and 57.9 decimal MB/s. Six hours is 1.25 decimal TB of originals, 2.5 TB for two copies before normal occupancy and headroom. To drain a six-hour backlog in a further six hours while new mail continues requires at least twice the original byte rate in useful upload throughput, before compaction and retries. These are arithmetic examples, not tested targets.

Measure full-system recovery from the externally observed fault time until mail reception and mailbox access work again. Include DNS/reconnect, RDS failover, node scheduling, surviving disk/network capacity, ownership recovery, and mailbox rebuild. Repeat with lost replies, partial failures, and active cleanup. A passing local model or managed-service typical failover duration cannot substitute for this test.

## Completion criteria for the next implementation stage

A prototype is ready for infrastructure evaluation when the format/restart, receipt guard, publication, cancellation, compaction, and pin tests run against real filesystem and PostgreSQL adapters; an S3 emulator is optional additional evidence. Provider-specific conditional-write and policy tests must then run against the actual target S3 service. Report the exercised versions, parameters, failure traces, and limitations. Production readiness additionally requires end-to-end IMAP compatibility, measured capacity, the AZ-loss recovery test, and migration/cutover evidence.
