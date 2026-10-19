# Explicit rotation and sealed-read validation

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Status: implemented local prototype; focused tests, full integrated race suite, vet and process restore passed. Production-scale and infrastructure recovery remain unvalidated. See the [design](../design/hot-file-rotation.md) and [ADR 0020](../adr/0020-seal-files-without-moving-message-locations.md).

## Scope

The slice introduces explicit local and authenticated remote rotation. It seals one bounded current file, initializes a new writer, and keeps existing targets, locators and conservative lane reservations unchanged. Sealed originals are read through bounded single-descriptor views with existing integrity checks. Rotation remains under the member-wide maintenance gate, reserves future rewrite capacity, and does not automatically trigger on admission failure.

The byte-image helper checks the actual opened descriptor’s identity/length and rejects non-regular paths without blocking on a FIFO. It creates no index, staging directory or recovery mutations. Local and remote reads resolve current authority rather than trusting a stale caller’s current FileID. An uncertain descriptor close retains read debt, freezes unsafe member operations and prevents a clean member/root-ownership handoff. Fault tests deliberately close a real descriptor and then simulate lost closure proof; they must not be described as an induced operating-system close failure.

## Focused read checks

Initial real-PostgreSQL race run 98633 failed before reaching sealed reads because the fixture used the admission minimum `2*C+H`; actual rotation correctly requires additional retained-source space while preserving a future rewrite file. The read fixture was increased to `3*C+H`. No production capacity check was relaxed.

Run 20582 then passed (`/tmp/fry-sealed-read-focused2.log`): receiptstore 3.765 seconds and hotstore 1.038 seconds, using:

```sh
./scripts/tests/receipts.sh -race -run '^TestSealed|^TestReadClosure|^TestFileImage|^TestMemberRead|^TestRemoteRead'
```

The checks cover actual local/remote rotation followed by sole-replica full/partial reads, a stale coordinator’s refreshed target authority, no fallback after a verified prefix, 32 active reads plus cancellation of the waiting 33rd, and local/remote closure-proof-loss handling with both zero and nonzero output. Fault cases verify retained pins, member freeze and refusal to release root ownership or mark a remote owner closed.

A small rotation health/status safety edit overlapped this run’s build, so it is read-path evidence rather than a frozen-source attestation for the final core. The final full suite will validate one frozen integrated snapshot. Source review also caught that replacing `Snapshot` with `Stat` would bypass the existing poisoned-byte-store health gate; rotation retains the health-checking snapshot and handles its close error conservatively.

## Focused rotation checks

Final core race run 20794 passed (`/tmp/fry-rotation-core-final2.log`), receiptstore 2.935 seconds. Earlier runs 39654 and 27185 passed the smaller matrices. Run 37244 stopped on a test-only response-code expectation: cross-generation status correctly returned 409, while action rejection used the existing 503 stale-generation mapping; the assertion was corrected without a production change.

Nine tests exercise repeated seals with unchanged complete target JSON, historical restart preflight, preparing-attempt exclusion, confirmed-abort physical census with unchanged worst-case reservation debt, spare-capacity refusal, registered empty-successor cleanup versus unexpected-payload preservation, recovery after published rotation without source unlink, remote operation replay/kind binding, actual clean-owner-generation status/action rejection, and the closed-byte-store snapshot health barrier. The health case tests the real snapshot gate; it does not claim an injected poisoned-store condition. Independent review found no remaining blocker in the implementation and these tests.

## Process restore across sealed and current files

The existing offline restore fixture now requests two real mTLS rotations on each member. Three incoming originals occupy the first sealed file, a dated IMAP APPEND occupies the second, and a queued outgoing original occupies the current file. The fixture checks operation status/replay and unchanged historical targets, then performs its normal shutdown, all-table/file backup, destruction of original resources, and fresh restore. Each independent replica must serve all five saved originals; new acceptance must still work. Four sealed files, four completed rotation jobs, twelve lane rows and exact physical charges are checked alongside the existing mailbox/security/queued-handoff assertions.

This fixture remains hot-only and synthetic. It does not establish crash takeover, AZ-loss recovery, production rotation latency, persistent-disk throughput, automatic rotation or sealed-file reclamation.


Initial process run 17846 stopped at Python’s strict TLS verification because the synthetic CA certificate lacked a key-usage extension. The harness now creates an explicit CA certificate with certificate-signing/CRL-signing usage; verification remains enabled. Production TLS behavior was not relaxed. Rerun 95861 passed with checked cleanup (`/tmp/fry-offline-restore-rotation2.log`).

The successful run verified five saved complete originals through each independent replica, one fresh original, four rotations with replay/status and unchanged targets, four sealed files, twelve exact lane censuses, two rejected archives and one queued relay handoff. The archive covered 18 files and nine directories. All-table/sequence equality was checked before starting the restored processes; member ownership generations were not rewritten to manufacture closure.

| Artifact | SHA-256 |
| --- | --- |
| [scripts/smoke/offline_restore.py](../../scripts/smoke/offline_restore.py) | `97c5dc00775cc749efb79bafd769a1e033a0d0462d00c028f2cfac5e345de8bd` |
| `internal/receiptstore/schema.sql` | `b726e5b8fc8cb1618bb398604421ec32edbd371ff44289813ffd2bfeb31b1ed0` |
| `fry` binary | `86c73c93a3f00409498c7488a5d562e796fea82ca7c5ff8605c79171f0dd42db` |
| `fry-member` binary | `475fa595ac0d3d4da9bb332a984754fa66b8254a5c931e893bd96425da381079` |


## Full frozen-source checks

Run 7525 passed `./scripts/tests/receipts.sh -race -short` against real PostgreSQL (`/tmp/fry-rotation-race.log`): receiptstore 78.263 seconds, SMTP 43.184 seconds. `go vet ./...` passed separately (`/tmp/fry-rotation-vet.log`). No implementation/test edits overlapped this final suite. Short mode omits the separate 200 MiB byte-store stress case; this slice preserves byte framing.

## Ordinary SMTP/IMAP follow-up

Run 35523 passed `./scripts/bench.sh load-mail 5000 65536 16 /tmp/fry-load-5000-rotation.json` with frozen source. The actual FRY process accepted all 5,000 messages with zero deferrals/failures, verified all 5,000 headers and eight complete-original/partial-read samples, and disposed of its synthetic fixture. Hot storage was repository-local Btrfs; both replicas shared that filesystem. The report SHA-256 is `34417c6870a2ee6a12c3216340a069b5f93aa56c7616bbec789bc981e3a35a23`.

| Metric | Observed |
| --- | ---: |
| SMTP elapsed / accepted per second | 11.630 s / 429.912 |
| Acceptance p99 | 53.922 ms |
| Last accepted → all visible | 7,099.474 ms |
| Cold / warm combined SEARCH | 8,541.353 / 239.286 ms |
| All-header metadata FETCH | 265.937 ms |
| Server peak RSS (FRY only) | 45,912 KiB |

The earlier member-catalog workload observed 417.220/s, p99 51.630 ms, 2,351.628 ms visibility lag, and 14,271 ms cold SEARCH. The new sample therefore has similar intake, a longer visibility backlog and a shorter cold query; these single observations do not establish causation or a stable regression/improvement. Keep the complete results rather than presenting only the favorable metrics.

This ordinary workload uses legacy uncapped files and does not measure rotation latency or sealed-file read throughput. The explicit-rotation process restore above covers correctness of that path. Neither test establishes sustained production rates, independent disk scaling, AZ failure or automatic reclamation.

## Member-owned read lifetime (2026-10-19)

The coordinator no longer holds the local gates of every target while it reads. `Store.readPinned` selects unremoved target members in member-ID order and owns fallback. Each member adapter now owns its read lifetime. The local adapter takes its context-aware read permit, then only its own gate, checks cancellation, closure, transport ownership and freeze under that gate, resolves the authoritative current or sealed locator and closes the descriptor before releasing either. The remote member-owned read keeps its independent pin registration and closure proof. Exclusive maintenance still takes only the gate, so the permit-before-gate order cannot invert. The gate wait is not cancellable; a canceled reader observes cancellation once it holds the gate. The fallback rules are unchanged: no fallback after emitted bytes, destination failure, cancellation or unproven descriptor closure.

There is one intended refinement. The coordinator no longer parses every target's locator before a read. A malformed locator on a replica that is never selected no longer fails a healthy read. A malformed selected locator fails that member before any bytes and may fall back under the rules above.

New PostgreSQL-backed tests in `member_read_ownership_linux_test.go` cover the following cases:

- An exclusively held gate on the unused replica does not delay the selected member's read. The selected member's live descriptor still blocks its own sealed reclamation.
- With every permit held by real sealed readers and reclamation queued, a permit wait honors its deadline. A reader that holds a permit while waiting for the gate is not released by cancellation; it finishes only after maintenance. A queued reader then reads the reclaimed successor exactly.
- When a member is closed while a reader waits for its gate, the read falls back to the replica before bytes. Cancellation after emitted bytes does not fall back.
- A mixed local/remote pair serves full, partial and current reads from either adapter after actual rotation.
- A malformed locator on the unused member is ignored; a malformed selected locator falls back; two malformed locators fail without output.

`TestShardReadRechecksRewrittenLocators` now rewrites the selected member after candidate selection, requires the rewrite to change its target file, and requires exactly one member attempt. It previously ran before the candidate query on a member with no dropped records, so the rewrite was a no-op. Mutations that restore caller-held gates or eager parsing of every locator fail the unused-gate and malformed-locator tests respectively.

The focused selection `FRY_TEST_PACKAGES='./internal/receiptstore' FRY_TEST_RUN='Read|Sealed|AutomaticRotation|RemoteMaintenance|MemberGRPC|SurvivingMember|Cold|StorageUsage|CleanupFairness' ./scripts/tests/receipts.sh -race` passed. The complete receiptstore package also passed with `FRY_TEST_TIMEOUT=30m FRY_TEST_PACKAGES='./internal/receiptstore' ./scripts/tests/receipts.sh -race -v` without `-short` (`118.449s`). This included the 200 MiB remote stream; only the three opt-in measurement tests skipped. The new and affected read tests also passed 15 consecutive race-enabled repetitions. All runs used disposable PostgreSQL 18.6 and Go 1.27.2. These are local results and make no throughput or latency claim.
