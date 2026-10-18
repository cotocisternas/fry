# Retained hot-file measurements

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

These local synthetic measurements quantify the current whole-file `Open` and
`Inventory` paths before a segmented-storage design. The completed runs below
passed on 2026-10-17. They are warm-cache, single-host measurements, not cold-disk
restart, crash recovery, production sizing, or availability evidence.

## Method

The opt-in [test](../../internal/hotstore/retained_measurement_linux_test.go) and
[runner](../../scripts/bench/hotstore_retained.py) use a new private fixture
under the repository's Btrfs filesystem. Report artifacts can live under `/tmp`;
the byte-store fixture and Go build scratch do not. The runner accepts only four
fixed workloads and refuses an existing output directory:

```sh
scripts/bench.sh hotstore-retained /tmp/fry-hotstore-retained-1024x1024-1
scripts/bench.sh hotstore-retained /tmp/fry-hotstore-retained-16384x1024-1 16384 1024
scripts/bench.sh hotstore-retained /tmp/fry-hotstore-retained-65536x1024-1 65536 1024
scripts/bench.sh hotstore-retained /tmp/fry-hotstore-retained-1024x65536-1 1024 65536
```

Setup performs real sequential `Append` calls, including their fsyncs, with
unique keys and uniquely identifiable payloads. It reuses one payload buffer
instead of retaining an array of originals. Setup is outside all reported phase
clocks. These are byte-store records, not accepted FRY receipts; PostgreSQL,
mailbox publication, read pins and authenticated replica transport are absent
from this part of the measurement.

After setup closes the store, each phase runs in a fresh child process:

- **Open:** time the complete existing-file `Open`, including scanning every
  frame, validating payload SHA-256/footer integrity, building the in-memory
  locator map, fsync and the directory/staging cleanup path. The fixture has no
  interrupted staging or incomplete tail.
- **Inventory:** open the store outside the clock, then time the snapshot copy
  of its existing in-memory locator map.

Every child independently verifies every key, locator count, file identity,
file length, message length and expected payload SHA-256 through full `Read`
calls after the clock and memory snapshots. Verification is not a sample and
does not rely solely on the scanner accepting its own file. The index and
Inventory slice stay reachable during the retained-memory snapshot.

`total_allocated_bytes` is the process's `runtime.MemStats.TotalAlloc` difference
around the phase. It is cumulative allocation, not peak or retained memory.
`retained_heap_delta_bytes` compares `HeapAlloc` after an explicit post-phase GC
with the baseline after a pre-phase GC. Both GCs are outside the phase clock;
the result includes small runtime/harness effects. Inventory's baseline already
contains the opened index. Immediate `HeapAlloc`, GC cycles, Linux RSS and
process-lifetime `VmHWM` are also recorded. RSS includes runtime/allocator memory;
child-lifetime HWM is **not** a measured phase peak.

Setup and verification warm the filesystem cache. The harness does not evict
cache or isolate a storage device. A preliminary `/tmp` tmpfs sanity run at
`/tmp/fry-hotstore-retained-sanity-1` is excluded from the Btrfs results below;
fsync on tmpfs does not establish persistent-disk behavior.

## Completed Open and Inventory results

Environment: Linux amd64, Go `go1.27.2`, 24 logical CPUs and `GOMAXPROCS=24`.
`GOGC` and `GOMEMLIMIT` environment values were empty. Each table row is one
completed run, without repetitions or confidence intervals. MiB means 1,048,576
bytes. Fixture removal and all per-child correctness checks passed.

| Records × payload bytes | Raw MiB | Encoded file MiB | Open ms | Inventory ms |
| --- | ---: | ---: | ---: | ---: |
| 1,024 × 1,024 | 1 | 1.168 | 2.316 | 0.043 |
| 16,384 × 1,024 | 16 | 18.688 | 24.223 | 0.775 |
| 65,536 × 1,024 | 64 | 74.750 | 95.378 | 2.761 |
| 1,024 × 65,536 | 64 | 64.168 | 26.260 | 0.063 |

| Records × payload bytes | Open retained heap delta MiB | Open allocated MiB | Inventory retained heap delta MiB | Inventory allocated MiB |
| --- | ---: | ---: | ---: | ---: |
| 1,024 × 1,024 | 0.276 | 0.657 | 0.087 | 0.094 |
| 16,384 × 1,024 | 4.272 | 9.107 | 1.491 | 1.500 |
| 65,536 × 1,024 | 17.027 | 36.116 | 5.991 | 6.000 |
| 1,024 × 65,536 | 0.276 | 0.657 | 0.087 | 0.094 |

In these completed cases, Inventory allocated exactly 96 bytes per record.
The 16,384- and 65,536-record Open retained-heap deltas were about 273 and
272 bytes per record, respectively,
including measurement noise. These observations identify record-count costs;
they do not justify extrapolating production RSS or selecting a segment size.
At the same 64 MiB raw payload volume, the 65,536-record Open retained about
17.027 MiB of additional Go heap, versus 0.276 MiB for 1,024 larger records.
Inventory allocated 6 MiB versus 96 KiB. This is consistent with the index and
snapshot growing by record count. Open time also differed, but the files have
different framing overhead and these are single warm-cache runs; the comparison
does not isolate disk I/O or establish a general restart rate.

## Evidence identity and cleanup

Completed artifacts are private local files:

- `/tmp/fry-hotstore-retained-1024x1024-1/report.json`
- `/tmp/fry-hotstore-retained-16384x1024-1/report.json`
- `/tmp/fry-hotstore-retained-65536x1024-1/report.json`
- `/tmp/fry-hotstore-retained-1024x65536-1/report.json`

SHA-256 of those reports, respectively:

```text
63a01f1c211ace65b2f81d9bf7b02c1ab1dea764cf6867e7532d3d889abfc65d
a7e22edb3d1c265dfbbc1b645f9af5dcd5bbd9f27d18e457d72b63ef32374335
e8e2e34aaf93381e7ca2eb2c43e4e9cd59b692397261ec7a9e22069bcf951d01
0ca2ceb1132944f231eee1f6abe24a119be4acf6f11aeea1f1e16dbc4217679f
```

Each directory also contains `open.json`, `inventory.json` and bounded `test.log`.
The reports record exact source hashes, the actual test executable hash, Go and
CPU settings, filesystem type, workload, encoded byte length, memory definitions
and complete verification counts. All four completed Btrfs runs used:

```text
test executable    ec8c385ffb85a751fe396dc268f7a1199e1945a509296b72d0060f5923f8488e
measurement test  5baf4b519f61612e4043e18e3d4dc5eb7e4f70e8fa417f43bc09836478828711
runner            7d363df92e89c26eca85f4dc9c9fa6596ba48a3979025f2e42d8cc172d9404e5
store_linux.go    46beff03792da6d185e673bf8d372c8caadf4b31fb1d6be9770b89420f441169
recovery_linux.go c241fad9f806e280334c10e29e9bc000d961b03a0a45764199b7715b28249ee9
format.go         06c359b4421c4eea1c74e1811bbb834299b838c04701ab05123e1f4daf47be5d
```

The child must exit successfully, including its checked store closure. The
parent checks removal of its byte-store fixture before reporting success. The
runner validates all counts and reports, removes fixture/build scratch, then
publishes evidence through a private staging directory and emits its success
summary only after checked cleanup. A failure preserves only a bounded diagnostic
log at the requested new output location and does not claim a successful run.

## Receipt-governed whole-member reclamation

The separate [test](../../internal/receiptstore/retained_reclaim_measurement_linux_test.go)
and [runner](../../scripts/bench/retained_reclaim.py) stage real accepted
originals into two local members with disposable PostgreSQL 18.6. The database
image is pinned to
`postgres@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336`.
These fixtures also use repository-local Btrfs scratch. Commands for the selected
cases are:

```sh
scripts/bench.sh retained-reclaim 1024 1024 /tmp/fry-retained-reclaim-1024x1024-1.json
scripts/bench.sh retained-reclaim 4096 1024 /tmp/fry-retained-reclaim-4096x1024-1.json
scripts/bench.sh retained-reclaim 1024 65536 /tmp/fry-retained-reclaim-1024x65536-1.json
```

The test uses one reused payload buffer with distinct original hashes. It
releases pending-mailbox references for alternating receipts through the public
API, lets the one-second recovery references expire naturally, then calls
`SweepExpiry` until all intended expired receipts become retiring. The other
half retain their pending references. No fake accepted receipts or physical
removal flags substitute for these transitions.

The measured window is the **whole first-member `ReclaimHot` call**, including
its receipt/target snapshot SQL, survivor copying, rewrite publication and
predecessor removal. It is not an independently measured exclusive-gate hold or
wait duration. Setup, second-member reclamation, final cleanup and verification
are outside this clock. `allocated_bytes_during_reclaim` is cumulative Go-test
process allocation; it excludes PostgreSQL and is not peak memory. Forced-GC
heap values before and after the call are retained process heap snapshots, not
an operation peak. The fixture retains receipt/dead/live bookkeeping; this part
does not use the hotstore measurement's fresh-child isolation.

After the clock, the test checks a changed file incarnation, removal of the old
generation, and an inventory containing exactly the surviving originals. It
reads **every survivor directly from the rewritten member**, checking its
accepted hash, so fallback to the second copy cannot hide an incorrect rewrite.
Database live target locators must equal those new locators, dead targets must
record removal, and the published member incarnation must match. The second
member is then reclaimed, `FinishCleanup` removes all expired receipts, and
logical retained count/bytes must equal the survivors. Member closure and fixture
cleanup are checked before evidence publication.

| Accepted originals × bytes | Live / expired | First rewrite ms | Cumulative allocated MiB | GC cycles | Forced-GC heap before / after MiB | Member file bytes before → after |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 1,024 × 1,024 | 512 / 512 | 1,086.196 | 117.238 | 42 | 2.226 / 2.121 | 1,224,768 → 612,416 |
| 4,096 × 1,024 | 2,048 / 2,048 | 4,209.007 | 468.833 | 82 | 4.466 / 3.963 | 4,898,880 → 2,449,472 |
| 1,024 × 65,536 | 512 / 512 | 1,251.982 | 117.264 | 43 | 2.263 / 2.151 | 67,285,056 → 33,642,560 |

The completed cases verified all 512, 2,048 and 512 live rewritten originals,
respectively, and removed the corresponding expired receipts after both
rewrites. Each is one warm-cache run on a shared local machine. Cumulative
allocation greatly exceeds retained heap; reading that allocation as required
simultaneous memory would be incorrect. The larger record count took about
4.209 seconds versus 1.086 seconds for 1,024 small originals. Keeping 1,024
records but increasing payloads to 64 KiB took about 1.252 seconds. This suggests
a record-count cost in the measured whole-call workload, without isolating its
cause or supporting an extrapolation.
These results do not yet isolate SQL round trips, copying, fsync or GC as the
cause of elapsed time, or establish a production rewrite-pause bound.

Completed report: `/tmp/fry-retained-reclaim-1024x1024-1.json`, SHA-256
`7274cbdf6954a180e93c894ba451977de4f44caa34278d8265f220c0315c6987`.
The 4,096-record report is `/tmp/fry-retained-reclaim-4096x1024-1.json`, SHA-256
`03dfa394191abd0e19a25dea291921aa5c85b66dd2df4304e89cc7e41a7f2ace`.
The 1,024 × 65,536-byte report is
`/tmp/fry-retained-reclaim-1024x65536-1.json`, SHA-256
`881acf69a27f4857c48bbcd491e17a8ed10786841249e7ddb1cfe7d9cf52ad02`.
All three record exact source hashes, environment, PostgreSQL version, filesystem,
input/output bytes and full correctness counts. All share the measurement test hash
`b72393a6d6823dfabf92c415868bc1429bf8388d497ace30a1600cfaa62f8969`;
runner hash is
`60124f5bb00bf2439c134c62c9305a75e057c55a43837e0503f89eb87eb78946`.

The remaining design constraints are described in the
[storage scaling proposal](../design/storage-scaling-next-step.md). No segmented
implementation or production capacity claim follows from these measurements.
