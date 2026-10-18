# Bounded hot-file admission validation

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Implemented local prototype; throughput work remains in progress. The [design](../design/bounded-hot-admission.md) and [ADR 0018](../adr/0018-reserve-hot-file-capacity-before-dispatch.md) define an opt-in capped single-file profile. Full files defer new attempts; automatic rotation is not implemented by this slice. Legacy zero-limit mode retains its existing behavior.

## Correctness evidence

Focused PostgreSQL race run 98906 passed: receiptstore 9.296 seconds and hotstore 1.045 seconds. The new tests cover:

- Record and byte reservations before reading concurrent append sources; identical retry at the cap, conflicting retry rejection, failed staging release, and lost sync reply occupancy.
- Full-file reopen and early scan limits that preserve existing data and staging files on rejection.
- Atomic two-member reservations, including rollback when only the second member is full; concurrent admission cannot overbook.
- Once-only settlement to actual encoded bytes, prepared retry after lease expiry, and accepted replay without extra charge.
- Aborted copies, absent copies and physical copies whose locator was never published; capacity remains reserved until closed-write census and rewrite.
- A deterministic near-capacity mixed-size rewrite in physical append order, with exact surviving bytes and new locators verified independently on both members.
- Old physical occupancy retained after publication until durable unlink; successor job reservation and aggregate budget checks.
- Frozen-profile mismatch, malformed ledger, and physical file size exceeding recorded occupancy, all refused before filesystem mutation with staging sentinels preserved.

An initial focused run exposed an ambiguous PostgreSQL subtraction parameter; explicit bigint casts fixed it. A later test attempted to resume a still-live preparation lease and correctly received `ErrBusy`; the fixture now expires that lease before testing resumed settlement. Neither failure is omitted from this record.

The full real-PostgreSQL race suite passed in run 38567 (`./scripts/tests/receipts.sh -race -short`), recorded at `/tmp/fry-bounded-admission-race.log`: receiptstore 70.854 seconds, IMAP 67.800 seconds and SMTP 42.652 seconds. `go vet ./...` also passed, with `/tmp/fry-bounded-admission-vet.log`. The separate-process bounded [offline restore rehearsal](offline-restore.md#follow-up-with-bounded-member-admission) also passed in run 32017 (`/tmp/fry-offline-restore-bounded.log`), including both independent replicas, frozen profiles and settled occupancy, then one fresh receipt after restore. Paired intake measurements follow below. Earlier catalog and retained-data results remain evidence of their recorded source snapshots, not measurements of bounded-mode overhead.

## Measurement scope

The acceptance runner now supports a third `legacy|bounded` argument. Both profiles use 256 MiB per member, 1,024 complete 64 KiB originals, 16 workers, 16 append slots and a prewarmed 16-connection PostgreSQL pool. The bounded fixture adds a 2,048-record and 96 MiB file ceiling. These are test settings, not proposed production limits.

The timer covers public `Store.Accept` only. All receipt identities and complete original bytes are verified afterward; bounded runs additionally verify all 2,048 settled target reservations and both exact file occupancy totals. Reports capture the actual member filesystem. A tmpfs result measures metadata/coordination and memory-backed I/O, not persistent-disk fsync performance. Short workstation runs cannot establish a production overhead bound, burst capacity, or an availability guarantee.

## Initial measurements and contention diagnosis

All five baseline runs below passed receipt/body and applicable capacity checks,
and published success only after checked cleanup. Hot members were on tmpfs;
PostgreSQL ran in its separate pinned disposable container. The first four used
the same frozen source and 64 KiB maximum, equal to the actual message size.
The fifth kept 64 KiB bodies but raised the maximum to 1 MiB, exercising actual
unused-reservation refunds. Its harness added that explicit size option; the
production capacity implementation remained unchanged.

| Run / profile | Elapsed ms | Accepted/s | p99 ms | Report SHA-256 |
| --- | ---: | ---: | ---: | --- |
| 18742 / legacy 1 | 4253.888 | 240.721 | 192.341 | `8897a02d4a9260b0f539fc5e162eea675f91bbd5bed73b26c3d2811d85fc6d8c` |
| 28277 / bounded 1 | 5913.025 | 173.177 | 246.180 | `42d1e91e2d34a2f282b5fb9bd17e70afb15f3656a3a59980965a51a37f2843e6` |
| 61715 / legacy 2 | 938.445 | 1091.167 | 24.944 | `3ebcacf50520902a6d800ee8721957ba2446df4f30ebfd60cbd7108d23abfd13` |
| 93022 / bounded 2 | 5866.397 | 174.553 | 252.405 | `b8c113c87fe7150acfc5df8f8fc4a2e4408f044a17372ef8419a87bc3ea2bc6e` |
| 9014 / bounded, 1 MiB maximum | 5494.359 | 186.373 | 230.246 | `04869cd69982f424ad100d8f9b9085439edbf3ea0e719fcf553e1a917be517da` |

Artifacts are under `/tmp/fry-acceptance-bounded-legacy-{1,2}`,
`/tmp/fry-acceptance-bounded-enabled-{1,2}`, and
`/tmp/fry-acceptance-bounded-wide-before-1`, each with `baseline/report.json` and
`run.json`. The large unchanged-legacy variation remains unexplained; do not
exclude its slow run or present the fastest legacy result as a stable baseline.
The repeated bounded results nevertheless warranted investigation before adoption.

Diagnostic profile run 22844 (`/tmp/fry-acceptance-bounded-profile-1/profile`)
passed at 5,692.471 ms with all 1,024 originals verified. SQL fingerprint mapping
to the frozen source found 31.010 seconds of overlapping time across 2,048
settlement file-row updates and 29.681 seconds across 2,048 preparation file-row
updates. Attempt-row locks accumulated 16.912 seconds and commits 8.273 seconds.
These concurrent durations are not an additive wall-clock breakdown, and exact
PostgreSQL wait types were not captured. They identify the shared file-accounting
path as the next investigation target, not evidence for disk or CPU tuning.

In the exact-maximum workload, every settlement file update had a zero byte
delta. The narrow optimization now avoids that exclusive write while still
settling the target, checking file authority and rechecking the lease. A real
PostgreSQL regression failed before the fix in run 86740: an exact-maximum
prepared write had durably appended bytes but waited on the file-budget row
held by another transaction, confirmed through `pg_blocking_pids`. The fixture
released the blocker and joined the writer before reporting failure. After the
fix, run 54463 passed the full focused race matrix (receiptstore 9.504 seconds;
hotstore 1.049 seconds); the same test checked the durable target and exact
original bytes while that unrelated file-row lock was still held. A separate
1 MiB-maximum workload is required because normal smaller messages still need
real refunds; improvement in the zero-delta case must not be generalized to them.

## Zero-delta follow-up

After the narrow fix, full race run 5198 passed (`/tmp/fry-bounded-admission-zero-delta-race.log`): receiptstore 67.319 seconds, IMAP 66.110 seconds, SMTP 42.897 seconds. `go vet ./...` passed separately. The earlier restore result precedes this narrow change; it is not relabeled as a fresh restore run.

Two sequential baseline measurements on the same frozen source passed all receipt, complete-body and capacity checks:

| Run / maximum size | Elapsed ms | Accepted/s | p99 ms | Report SHA-256 |
| --- | ---: | ---: | ---: | --- |
| 57021 / 65536 bytes | 2818.860 | 363.267 | 152.550 | `e839b1a64d7898514db953d20b4ea0b95a67d0d3c532a78c73614d0a056bd896` |
| 99710 / 1048576 bytes | 5759.094 | 177.806 | 250.243 | `203ec9f8fa1f9d62b490ffb6480f2a7b25c461a824388f8e6d10c7c1f4d749fd` |

Artifacts are `/tmp/fry-acceptance-bounded-zero-delta-after-1` and
`/tmp/fry-acceptance-bounded-wide-after-1`. Exact-size intake improved in this
local follow-up and the deterministic lock regression proves removal of that
unnecessary wait. Genuine-refund intake remained slow: this is not a general
throughput fix. Partitioning file accounting is the next design investigation;
no striped-budget result is established by these measurements.

The runner's fourth optional argument selects maximum size `65536` (default)
or `1048576`, while every original remains exactly 65,536 bytes. For example:

```sh
./scripts/bench.sh acceptance baseline /tmp/fry-capacity-refund-NEW bounded 1048576
```

## Combined-statement experiment

The next candidate keeps exact global file budgets. Fresh preparation combines
its file debit and target stamp using dependent data-modifying CTEs; genuine
refunds similarly pass returned post-update totals into the target stamp. Both
mutations must affect exactly one row. Locator publication remains a separate
preceding statement, now returning the reservation fields already locked there.
No target row is updated twice in one statement, and deadline checks stay after
the last locking write. The zero-delta path still avoids a file update.

Focused race run 43168 passed (receiptstore 10.068 seconds, hotstore 1.050 seconds).
Four new fault cases force a raised SQL error or a suppressed target update
during both reservation and refund. Nontransactional fixture witnesses confirm
the fault was reached despite rollback. Each case verifies complete accounting
rollback, retained uncertain bytes where applicable, and once-only settlement
on retry. This covers suppressed updates that an SQL-error-only test would miss.
Run 39726 passed the real-refund baseline with the 1 MiB maximum and 64 KiB
originals: 5463.882 ms, 187.413 accepted/s,
p99 214.813 ms. All 1,024 originals and 2,048 target
reservations passed verification. The report is
`/tmp/fry-acceptance-bounded-cte-wide-1/baseline/report.json`, SHA-256
`aa9fbfd35d9c2f54b599186d41ff1167a59db630d4f077ffc02f46c365fd11c9`. This is essentially the same
observed range as earlier real-refund runs; it does not establish a throughput
improvement. The reviewed candidate remains a comparison point while
[independent capacity lanes](../design/hot-capacity-lanes.md) are prototyped.
The full suite and restore results above predate the combined-statement change;
the new candidate's correctness evidence is the focused race/fault matrix.

## Partitioned-accounting follow-up

The [capacity-lane validation](hot-capacity-lanes.md) records the next adopted local prototype: independent reservation counters, confirmed-rollback-only retries, fragmentation tests, a fresh full race suite/restore and comparative exact-size/refund runs. The historical measurements and source snapshots above remain unchanged.

## Remaining limits

This bounds each enabled member's current file and retained record index, at the cost of refusing new receipts when that file is full. Reclamation still pauses the member. Unknown-write debt can reduce usable capacity until closure and census; it is deliberately not refunded on timeout alone. There is no automatic rotation, sealed-file read path, schema conversion, repair or automatic crash takeover. Production AZ-loss recovery, sustained traffic, six-hour object-outage buffering and the configurable physical-erasure target remain separate gates.
