# Hot-file capacity partition experiment

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Status: adopted for the local bounded-member prototype after correctness checks, restore and comparative measurements. This extends the [bounded-admission experiment](bounded-hot-admission.md) and implements the [immutable capacity-lane design](../design/hot-capacity-lanes.md). Independent counters replace the current file’s shared reservation counters; physical file and rewrite charges retain their previous meaning. No production throughput, automatic rotation or failure-domain guarantee is established here.

## Scope and invariant checks

The layout fixes at most 16 record/byte partitions per bounded file. A target keeps its assigned partition through retries and surviving rewrites. Both replica reservations commit together. A lost capacity race can retry only after explicit successful rollback of the complete preparation transaction; eight unsuccessful attempts return busy. A fresh snapshot with no eligible partition reports capacity, including conservative fragmentation where unused aggregate bytes or slots remain.

The comparative harness still accepts 1,024 originals of 64 KiB with 16 workers and one local replica pair. Exact-size and 1 MiB-maximum variants separate zero refunds from real refunds. Setup, full-body verification and lane/target accounting checks occur outside timing. Current reports verify the derived layout, global ceiling sums, all settled targets and each lane’s target census. The production ceiling/count interface has no additional lane tuning knob.

## Initial failed test and diagnosis

Focused race run 3627 failed two cases (`/tmp/fry-capacity-lanes-focused.log`). The layout-negative fixture attempted to set lane version 2, which the database correctly rejected through its version constraint; the fixture was changed to exercise legal-but-contradictory layout metadata instead. The zero-delta progress fixture held the assigned lane with `FOR UPDATE` and observed a real PostgreSQL lock dependency after durable append.

Diagnostic run 85711 reproduced that second failure (`/tmp/fry-capacity-lane-fk-lock.log`). The blocked statement was the target settlement update, with `wait_event_type=Lock`, `wait_event=transactionid`, and the held lane transaction identified by `pg_blocking_pids`. The new target-to-lane foreign key can require a key-share check when the same transaction has already updated that target’s locator. An artificial `FOR UPDATE` parent lock blocks that check; a normal non-key capacity-counter update uses a compatible lock mode. The production foreign key is retained. The progress fixture now holds a real no-op counter update, so an unnecessary exclusive counter write would still block and fail the test.

The earlier single-counter zero-delta red/green evidence remains in [bounded-hot-admission.md](bounded-hot-admission.md); these are distinct schema and fixture snapshots.

## Focused concurrency and failure checks

Final focused run 33456 passed with real PostgreSQL and the Go race detector (`/tmp/fry-capacity-lanes-final2.log`): receiptstore 13.377 seconds, hotstore 1.053 seconds. Run 82298 had first stopped at a test-only array/slice compilation error, corrected before this run. Production source stayed frozen. The final implementation and test source review found no blocking findings.

```sh
./scripts/tests/receipts.sh -race -run '^TestMemberCapacity|^TestCapacityTarget|^TestBoundedHot'
```

The focused checks establish:

- A genuine refund in another lane completes while a first refund is confirmed PostgreSQL-blocked; a zero-delta settlement completes while its own lane is held by a real non-key counter update. Both verify committed targets and direct original bytes before releasing the blocker.
- An actual second-member capacity race waits on a chosen lane, observes that lane become full, rolls back the first member’s debit and retries through another lane. Exact counters and both original copies are verified afterward.
- Suppressed reservation updates on either the first or second member drive exactly eight complete preparation attempts, then busy. Neither byte store, target authority nor counters retain partial work. A simulated lost rollback reply returns uncertainty and excludes the private retry marker.
- Seventeen actual accepted messages create disjoint record/byte exhaustion across two lanes. Even though aggregate spare slots and bytes exist, another acceptance defers without leaking reservations. This is the documented fixed-partition tradeoff, not physical disk exhaustion.
- Integer ceiling remainders neither create nor lose aggregate budget; full lanes permit fallback to eligible lanes. Retry/reopen preserves lane identity. Rewrites preserve survivor lanes and exact direct bytes, and zero old lane occupancy only after cleanup.
- Contradictory profiles/layouts and physical-byte under-accounting fail before filesystem mutation. Existing both-replica atomicity, uncertain-copy debt, publication/unlink cuts and once-only settlement regressions remain covered.

## Full local checks

Full real-PostgreSQL race run 98221 passed (`./scripts/tests/receipts.sh -race -short`, `/tmp/fry-capacity-lanes-race.log`), including receiptstore in 67.926 seconds and SMTP in 41.560 seconds. `go vet ./...` passed separately (`/tmp/fry-capacity-lanes-vet.log`). Short mode omits the separate 200 MiB record stress case; the lane slice does not change the byte framing or stream implementation. These checks and the restore below use the frozen candidate production source.

## Offline restore

Run 92010 passed the complete hot-only restore rehearsal and checked cleanup (`/tmp/fry-offline-restore-capacity-lanes.log`). Both remote members use 128 records, a 20 MiB file ceiling, 64 MiB member capacity and a 1 MiB message maximum; this derives two lanes per file and preserves staging/rewrite headroom. The file ceiling differs from the earlier 16 MiB bounded restore fixture so this run exercises a multi-lane layout.

The rehearsal stops and proves closure of every original writer, backs up authority plus member files, deletes the original database and roots, and restores into fresh resources. It verifies all five saved originals through each independent replica, one new accepted original, exact lane/target occupancy before backup and after new acceptance, mailbox state, sender enforcement and one queued synthetic relay handoff. Both negative archives are rejected. All-table/sequence snapshot comparison includes the lane table. Ten files and five directories are covered by the byte manifest.

| Artifact | SHA-256 |
| --- | --- |
| [scripts/smoke/offline_restore.py](../../scripts/smoke/offline_restore.py) | `b8eed2b8aaa3cced9b17e8904146b6b338ae7108c7b5bc04eb5cb37cde4e463a` |
| `internal/receiptstore/schema.sql` | `8bcaea9256ea34271e5a3065d94731061c055ac8e91164e17afdb95434df3b85` |
| `fry` binary | `0622ae71b2f7401b33eeaa2f871ad80d07c879c7632b3f3cdf1bbc68a8ea8bd0` |
| `fry-member` binary | `77a99c6c89d3203b521b29d79ea451b906e0dc1321fb1328c274a05944e7e400` |
| `fry-admin` binary | `24004117cdcd18cba8d5ee8841d12027ae6a830aec98a30a6f7ff0b20e637221` |

This remains an offline hot-only test with synthetic mail and separately retained secrets. It does not exercise cold-pack restore, historical rewrite tombstones in the archive, online backups, crash takeover or AZ-loss recovery. See [offline-restore.md](offline-restore.md) for the complete recovery scope and limitations.

## Comparative intake measurements

Three sequential untraced runs used identical frozen source and successful checked cleanup. Each accepted and verified all 1,024 receipts and complete originals, all 2,048 settled targets, exact file budget totals and every lane’s target census. Bodies remain 64 KiB; only the configured maximum changes. The derived layout is 16 lanes for the exact-size case and 11 for the 1 MiB maximum.

| Run / maximum bytes / lanes | Elapsed ms | Accepted/s | p99 ms | Report SHA-256 |
| --- | ---: | ---: | ---: | --- |
| 49473 / 65536 / 16 | 1063.953 | 962.449 | 29.383 | `2a7d039721544fdedefc6cff1b87cf49c5ef81372f5ee8776dfb1b77f8d580f1` |
| 33284 / 1048576 / 11 | 1407.294 | 727.637 | 41.042 | `37c57987ef45cde04374a27125fe60aafc1bcf1332387965c249d9e6ba574f97` |
| 74147 / 1048576 / 11 | 1816.816 | 563.623 | 56.367 | `9272c2b67fc60de94656c64ef949100237b660028190d68b1f0f129d9c7b7f19` |

Evidence directories are `/tmp/fry-acceptance-capacity-lanes-exact-1` and
`/tmp/fry-acceptance-capacity-lanes-refund-{1,2}`, each with `baseline/report.json`
and checked-cleanup `run.json`. The runner SHA-256 is
`4e37d7c98a239a7aadc15891ed8d5eadbcc37f05d5ef01d1c9d3d3e0f0d80126`.
Reports contain identical production/test source-hash sets.

The preceding combined-query single-counter refund run measured 187.413/s and
p99 214.813 ms. Both lane refund runs improved on that observation, at 727.637/s
and 563.623/s, with p99 41.042 and 56.367 ms. The changed second result is retained;
these short runs do not estimate a stable speedup or production capacity. The
exact-size observation improved from the prior zero-delta-only 363.267/s to
962.449/s. The deterministic lock tests establish independent progress; timings
show that it matters for this local workload. Neither statement implies arbitrary
workload utilization or linear scaling with lane count.

All hot-member roots were reported as tmpfs. PostgreSQL ran separately in the
same pinned disposable Docker image. CPU, filesystem caching and shared-host
variation are not controlled. These results do not measure persistent hot-disk
fsync, cross-host replication, SMTP-to-IMAP completion, sustained backlog drain,
AZ loss or the required production traffic envelope.

## Decision and limits

Adopt immutable lanes for the bounded-member prototype under [ADR 0019](../adr/0019-partition-hot-file-capacity.md). The measured refund improvement and independent-progress regression justify the additional accounting rows. Preserve the explicit fragmentation tradeoff and the one-lane fallback for small-file/large-message profiles. No capacity borrowing is implicit.

Automatic rotation, sealed-file reads, schema conversion, crash recovery and production resource sizing remain outstanding. A bounded current file still defers new mail when no eligible lane remains; accepted originals keep their existing lifetime rules. The next storage slice adds explicit rotation with authoritative sealed-file reads and spare rewrite capacity before relaxing maintenance concurrency.


The subsequent [explicit rotation validation](hot-file-rotation.md) adds sealed-file reads and a fresh restore across sealed/current files. The measurements above remain evidence for the earlier lane snapshot, not rotation performance. Automatic triggering and sealed-file reclamation remain separate work.
