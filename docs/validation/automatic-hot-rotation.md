# Automatic hot-file rotation validation

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Status: local integration validation passed on 2026-10-17. See the [design](../design/automatic-hot-rotation.md) for exact eligibility, remote cache behavior and scheduling limits.

Focused PostgreSQL-backed race run `46123` passed (`3.746s` receiptstore), followed by expanded run `6430` (`4.413s` receiptstore). Logs: `/tmp/fry-automatic-rotation-focused.log` and `/tmp/fry-automatic-rotation-final.log`. The latter includes direct preparing-target and confirmed empty-abort cases, wrong-kind and old-owner rejection, stale cached-current recovery on a later turn without identity RPCs, nonblocking refresh-mutex contention, durable no-op replay after later fills/rotations, and physical-headroom rejection.

Independent tests keep an actual reader descriptor open and queue an explicit rotation writer, then require local and remote conditional calls to return busy before that reader is released. This detects an accidental gate-dependent identity request or generic HTTP read lock before the nonblocking gate attempt. Other tests exercise completed refunds restoring lane capacity, resumed admission after actual full-file rotation, and historical full/partial reads with only one replica available. Scheduler tests verify one action per opportunity, preserved cleanup turns despite busy/stale/no-op results, disabled-by-default rotation and the reclamation configuration requirement.

The complete `./scripts/tests/receipts.sh -race -short` run passed, session `43784`: receiptstore `91.155s`, SMTP `42.490s` (`/tmp/fry-automatic-rotation-race.log`). `go vet ./...` passed (`/tmp/fry-automatic-rotation-vet.log`). The short suite excludes the separate 200 MiB stress case.

## Real-process SMTP/IMAP rehearsal

`scripts/smoke.sh automatic-rotation` builds the real coordinator and two mTLS members with a four-record bounded profile. It initially disables automatic rotation, fills both current files, verifies the next attempt has not been accepted, enables scheduled rotation through a normal coordinator restart, and retries until twelve originals have been accepted. It then disables maintenance and verifies all exact mailbox originals through each sole replica, followed by clean process closure and exact lane/target accounting. Temporary resources and synthetic secrets use the existing offline-rehearsal fixture and its checked cleanup.

The first run, `7098`, failed with `SMTPServerDisconnected`. Instrumented repeat `83171` reproduced the failure exactly at the fifth DATA transaction against the full file. Logs: `/tmp/fry-automatic-rotation-runtime.log` and `/tmp/fry-automatic-rotation-runtime2.log`. Source diagnosis identified existing behavior: `Store.Accept` closes its unread input on preparation failure, and the SMTP source wrapper closes a DATA socket before the SMTP library can drain framing and send 451. No production code was changed for this finding.

The harness now distinguishes explicit temporary replies from disconnects. Before retrying either, it checks that no preparation remains and that the exact synthetic original has no accepted receipt. This authority check is possible only in the isolated fixture. A real SMTP client cannot infer non-acceptance from a connection reset; a lost reply after acceptance remains ambiguous and ordinary retry can duplicate mail. That revision did not provide an explicit 451 or reusable connection for pre-read capacity rejection. A subsequent [bounded DATA deferral repair](smtp-capacity-deferral.md) has passed focused race tests; its full-suite and repaired runtime validation are tracked separately. The historical process results below retain their original disconnect behavior.

Corrected runs `35329` and `87717` resumed intake, accepted twelve originals and verified their exact bodies through both sole replicas. They failed afterward in a reused fixture cleanup assertion: the offline helper expected two closed owner generations, while the deliberate replica restarts correctly produce four. The helper now takes an explicit expected count, defaulting to two for its original offline scenario; the automatic-rotation rehearsal expects four. The requirement that all owners and actual read handles close remains unchanged. Logs: `/tmp/fry-automatic-rotation-runtime3.log` and `/tmp/fry-automatic-rotation-runtime4.log`.

The final corrected rehearsal passed, session `93053`, with checked cleanup (`/tmp/fry-automatic-rotation-runtime5.log`): twelve accepted originals, four completed rotations, two independently verified replicas, and exact final lane accounting. There were 69 unsuccessful attempts, all disconnects with proven non-acceptance in the fixture, and zero explicit 4xx replies. These retries are synthetic rapid probes of capacity recovery, not a recommended production SMTP retry policy or throughput measurement.

Validated hashes:

```text
schema.sql                      dbda2fd47b497ab57f1ce9ea057aa2ce808ca13e1637eff326fae5c44071cd25
automatic_rotation_linux.go     f2e782e848813b6ab1df06040b7d559b3eee8b9b81b2b70a2a6bd06a26106cf6
cmd/fry/lifecycle.go             19d69bcba69764b441543dd3aaa26b5ecf17234bbe35234bf35145d991b2d898
scripts/smoke/automatic_rotation.py  2e6a795a756d7d9cbee11ba9b6b2df0de9825b60f87179061864a86b8b22e089
scripts/smoke/offline_restore.py     7fdcb752d4ddadea78e42c114c90f31eba97920cdcb8b696680a8a462c717a3b
fry binary                      471b8d7b01cc9f8f17042c674f5c91b658fcad21226969ecfa20d07372e2778e
fry-member binary               12e1001e421b70dc0f7fbe861a4bc58b6ba9a4ed8404ebea946305678cd68222
```

No performance or production recovery claim is made. In particular, repeated busy readers can still starve conditional rotation, the scheduler interval adds capacity-recovery delay, and no cloud/AZ or sustained-burst workload is covered here.

The existing offline hot-only restore rehearsal also passed on this schema and these binaries, session `11261` (`/tmp/fry-offline-restore-automatic-rotation.log`): five saved originals through both independent replicas, one fresh original, four explicit rotations, two sealed reclaims, preserved removal of one expired original, two rejected archive variants and one restored queued handoff. Its 20-file/11-directory archive and cleanup checks passed. The configurable expected-owner count defaults to the original two-owner scenario in this run. [Offline-restore exclusions](offline-restore.md) still apply.

## Rehearsal after the SMTP deferral repair

Session `82571` passed after the [bounded pre-read DATA repair](smtp-capacity-deferral.md),
with twelve accepted originals, four rotations, both sole-replica body checks,
final accounting and cleanup. All 69 unsuccessful attempts received explicit
temporary replies; none disconnected. The harness now requires `451` for the
initial full-file attempt with rotation disabled. Evidence:
`/tmp/fry-smtp-preread-runtime.log`; the linked repair document records the new
source and binary hashes. This replaces the transport limitation for proven
pre-read rejection with complete bounded framing, without changing uncertain
acceptance semantics or claiming a production retry/throughput result.
