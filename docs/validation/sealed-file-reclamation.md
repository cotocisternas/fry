# Sealed-file reclamation validation

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Status: local validation passed on 2026-10-17. The [design](../design/sealed-file-reclamation.md) adds one-file sealed reclamation to the local prototype. No AWS resources or production messages were involved.

## Implementation scope

Reclamation leaves the active writer unchanged and either publishes a sealed replacement containing survivors or records an explicit delete-only outcome. Each operation durably selects one source; retries cannot choose a different file. Source charges remain until verified closure and durable unlink. The existing maintenance schedule alternates current/sealed member sweeps, and file selection uses a bounded indexed seek/wrap rather than sorting the full sealed catalog.

Inventory reads one exact immutable file with record, encoded-byte and message-size caps. It returns a complete physical-order census or an error with no usable prefix. Snapshot inventory preserves the separate immutable-prefix contract when its active writer later grows. Neither path performs staging recovery or retains an index on the image.

## Findings and corrections

Independent review found a lock-order inversion in the first implementation: maintenance held the exclusive member gate and waited for a read permit, while queued remote readers could hold every permit and wait for that gate. Exclusive maintenance now opens its single image without using normal-reader permits; the gate has already drained actual readers. A deterministic regression exhausts the reader permits before reclamation and verifies progress without consuming them. This does not relax normal read concurrency limits.

The first focused PostgreSQL-backed race run, session `93303`, failed one closure-proof assertion. The test compared the root directory's size before and after legitimate creation of a tracked unpublished successor. Its source-byte, target, charge and freeze assertions passed. The fixture was corrected to preserve exact regular-file checks and directory modes without treating parent-directory growth as source corruption. Evidence: `/tmp/fry-sealed-reclaim-focused.log`.

The small inventory race suite passed, session `61698` (`1.035s`), reported by its implementation agent. Scheduler opportunity testing also passed through `go test ./cmd/fry -run '^TestHotReclamationSchedules' -count=1`.

The corrected focused run passed, session `59547`, receiptstore `10.004s`, followed by the expanded final focused run, session `1459`, receiptstore `10.508s`. Logs: `/tmp/fry-sealed-reclaim-focused2.log` and `/tmp/fry-sealed-reclaim-final.log`. Both used disposable PostgreSQL 18.6 and race detection. The final run includes mixed survivor and delete-only publication/unlink recovery cuts, direct reads from successfully reclaimed successors, physical count/length mismatch refusal, confirmed aborted writes both absent and present with a lost locator, operation replay/kind binding, new-owner rejection, finite-set selection past live files, and preservation of unexpected delete-only paths. Delete-only processing also succeeds while another transaction holds the profile row lock used for replacement-capacity reservation, demonstrating that the no-successor branch does not reserve replacement capacity.

The complete `./scripts/tests/receipts.sh -race -short` run passed, session `30746`: receiptstore `89.613s`, SMTP `44.799s`. Evidence: `/tmp/fry-sealed-reclaim-race.log`. Source remained frozen during that run. `go vet ./...` also passed (`/tmp/fry-sealed-reclaim-vet.log`). The short suite omits the separate 200 MiB stress case; no new large-message throughput claim is made.

The final reviewer distinguished direct survivor-byte verification after successful reclamation from authority-only assertions after interrupted publication. A subsequent test-only addition directly opens and reads the authoritative reclaimed successor after reopen in both published and unlinked failure cuts, with no replica fallback. Its targeted race run passed, session `40062`, receiptstore `2.089s` (`/tmp/fry-sealed-reclaim-recovered-bytes.log`); production code was unchanged after the full-suite run.

## Real-process fresh restore

`./scripts/smoke.sh offline-restore` passed, session `96397`, with checked cleanup (`/tmp/fry-offline-restore-sealed-reclaim.log`). The fixture receives an extra synthetic original, seals it alongside three survivors, expunges it through IMAP, and moves only its recovery deadline into the past to simulate elapsed retention. The real scheduled expiry worker establishes retirement. Actual mTLS sealed-reclaim/status/replay calls remove both predecessor copies without changing either current writer identity.

The fixture then completes its normal additional rotation, dated APPEND and queued SMTP handoff, cleanly stops all writers, backs up the entire authority and member roots, destroys the originals, and restores into a fresh PostgreSQL/container and new root paths. All five saved originals are verified through each sole replica, one fresh original is accepted, and the expired receipt and both removed physical copies remain removed. It also verifies exact live lane occupancy, physical charges, two completed sealed-reclaim histories, four sealed files, four rotations, two negative archive cases and one queued handoff. The archive contains 20 files and 11 directories. See the [offline-restore scope and exclusions](offline-restore.md).

Source hashes at this validation point:

```text
schema.sql                 db57f06dbea78e5fde25406c00aa871897a955df77f3887d2a9f9075d186f980
sealed_reclaim_linux.go     e04fec3ea44df3cc10718200e2a28d3f6df76241b8baa20590f4f526318a13f0
inventory_image_linux.go    10f57f6491f8a4f69287b9d790bb36318da38cf00b972a5af72c5f8bbf363388
cmd/fry/lifecycle.go        56b221c97d3297a49bc45b2c0eccb2534948cba37dc19f462121ad80bbfc32b0
scripts/smoke/offline_restore.py     a773beaba2b241aab6813ee36c7992f6bbb507841f70b8c2225cbc186dc28b88
fry binary                 4ba52acc34e664a53d0b058670ad66e67a4e8e61f1416897c7e547e4a82d7c30
fry-member binary          2fc6b154d7512fb905c30c6aec572bd464bf345a038920e80959b702aaffab45
fry-admin binary           24004117cdcd18cba8d5ee8841d12027ae6a830aec98a30a6f7ff0b20e637221
```

## Scope limits

Failure cuts and lost-close-proof tests use deliberate local fault injection. A synthetic close-proof loss closes the real descriptor successfully and then returns an injected error; it does not claim an observed operating-system close failure. Clean reopen is distinct from unclean-owner takeover, which remains unsupported.

Per-file caps bound work and memory but do not establish a latency SLA for a full file. The member-wide gate pauses its reads and writes during reclamation; reads served by its replica do not wait for it (see [member-owned read lifetime](hot-file-rotation.md#member-owned-read-lifetime-2026-10-19)). Cursor fairness applies to a finite stable set during one process lifetime, and scheduled opportunities share a bounded cycle deadline with other lifecycle work. No claim is made here for 24-hour erasure, six-hour S3-outage capacity, a 500,000-message burst, multi-AZ recovery, or production throughput.
