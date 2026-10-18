# Mailbox publication grouping experiment

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

**Batching was not adopted.** Production retains individual publication, restored
to `mailbox_linux.go` SHA-256
`5b84e9036d18e620f73a0ea2b4cfad336932ef0b25cca9e5800477108376e8f7`.
Incoming acceptance takes priority and temporary publication backlog is allowed.
Group16 improved drain visibility but consistently worsened acceptance in the
observed simultaneous-load runs. Group4 results varied and neither run completed
header verification. These results do not justify replacing the existing path.
The exact group16 implementation and fault tests remain an **unapplied**
[experiment patch](../experiments/mailbox-group-commit.patch); its dry run against
the restored source passed. Implementation descriptions below are historical.

## Isolated drain harness and verification

Measured on 2026-10-17 with:

```sh
scripts/bench.sh mailbox-publication /tmp/fry-publication-baseline.json
scripts/bench.sh mailbox-publication /tmp/fry-publication-group-1.json
```

The opt-in `TestMailboxPublicationMeasurement` skips unless
`FRY_PUBLICATION_MEASUREMENT=1`. The wrapper runs only this test against a fresh
disposable PostgreSQL container on numeric loopback and real local members.
It uses no existing database, SMTP, IMAP, cloud resources or production config.
Each of four fresh-schema cases accepts 1,024 distinct 1,024-byte originals
through public `Store.Accept`, with two local copies and pending mailbox
ownership, all targeting one organization and INBOX. Only after all originals
are staged does the drain clock start. Public `PublishMailboxes` is called with
limit 1, 16, 64 or 128 until all memberships are published. Selection/wrap queries
and publication transactions are timed; acceptance and verification are not.

Outside that clock, each case checks UID1–1,024, UIDNext1,025, receipt order for
this sequential fixture, unchanged dates/empty flags, and every complete original
through public `Store.Open`. It checks no remaining work, one live mailbox
reference per membership, expired pending references with release tombstones,
retained recovery references, and an empty subsequent publication turn.
**All 4,096 complete originals were verified in each measured run**, not sampled.
The full baseline test passed in 74.75 seconds and candidate in 70.892 seconds,
including staging and verification. Collaborating agents paused other heavy work.

Staging, publication and reads share a 180-second context. Fixture setup helpers
have their own background contexts; Go test is bounded at 240 seconds and the
wrapper's Go command at 300 seconds including compilation. The hardened wrapper
bounds database removal at 20 seconds and local cleanup at 10 seconds, checks
cleanup before final report publication, and bounds best-effort failure cleanup.
This hardening followed the baseline; syntax/offline success-and-failure proofs
passed without repeating unchanged work. Baseline cleanup was independently
checked and left no matching database container; candidate used checked cleanup.

Environment: Go 1.27.2, Linux/amd64, 24 logical CPUs, PostgreSQL 18.6 (Debian
18.6-1.pgdg13+2), pinned image
`postgres@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336`.
Members share one workstation's storage, not independent physical failure domains.
These are individual samples, not controlled repeated statistical estimates.

| Limit / public calls | Individual drain / published per second | Group ≤16 drain / published per second | Individual WAL | Group ≤16 WAL |
| --- | ---: | ---: | ---: | ---: |
| 1 /1,024 | 2,832.797 ms /361.480/s | 2,901.774 ms /352.888/s | 1,826,536 B | 1,827,040 B |
| 16 /64 | 1,867.156 ms /548.428/s | 521.658 ms /1,962.971/s | 1,818,896 B | 1,820,968 B |
| 64 /16 | 1,915.458 ms /534.598/s | 512.605 ms /1,997.640/s | 1,847,256 B | 2,111,600 B |
| 128 /8 | 2,009.387 ms /509.608/s | 1,604.240 ms /638.308/s | 1,853,512 B | 1,830,312 B |

WAL is the `pg_wal_lsn_diff` between `pg_current_wal_insert_lsn()` observations
around drain. It covers the whole isolated cluster, including possible background
activity/full-page images, and excludes later original reads/verification writes.
It is not exact per-operation accounting. No transaction count was instrumented;
asynchronous `pg_stat_database` counters were not used. Source inspection shows
one synchronous transaction per successful destination in the baseline. Changing
its public limit alone does not implement group commit.

Limits 16/64 improved approximately 3.58/3.74 times in these samples; limit 128
improved only 1.25 times and limit 1 was slightly slower. Results are non-monotonic
and their shape is unattributed. Do not claim uniform fourfold improvement or an
optimal production limit. Selection, round trips, commits, locking, CPU and cache
effects are not isolated by this table.

## Candidate correctness and boundaries

The experiment grouped at most 16 adjacent default-INBOX items for the same
organization and inbox inside an existing organization turn. Selection/cursor
bounds remained unchanged and the selection gate was released before publication.
All distinct receipt guards were acquired in sorted immutable-ID order before
any folder lock; UID allocation retained selection order. Existing per-item
publication logic kept membership, UID allocation, reference release and work
deletion atomic. Explicit-folder work, inline APPEND and staged migration kept
their existing paths and authority guards.

Only an acknowledged precommit rollback allowed individual fallback, preserving
healthy-item progress and existing error/backoff handling. After commit was
attempted, an error allowed neither fallback nor counting the group as known
published. Durable work/membership state remained replay authority.

The focused PostgreSQL race run passed (`receiptstore` 9.595s,
`imapmigrate` 7.011s); independent review found no P1/P2 issue. Tests covered 41
items split 16/16/9, overlapping workers and mutation lock order, whole-batch
snapshot visibility, explicit-folder fallback, UID exhaustion, rollback/fallback,
and uncertain commit replay. The ambiguous-reply test commits against real
PostgreSQL then synthesizes a returned error at the transaction seam; it does
**not** drop a real network packet. Correctness evidence did not establish an
acceptable end-to-end performance tradeoff.

## Simultaneous SMTP and publication observations

All six runs used 5,000×64KiB, 16 sequential SMTP workers and one local pair,
with the same coordinator's 16-connection pool shared by acceptance/publication.
Every run observed 5,000 accepted SMTP transactions, zero deferrals/failures and
all 5,000 visible memberships. PASS additionally required exact all-message
header/metadata/flag verification and eight complete-original plus partial-read
samples. Failed-verification rows are partial evidence, not full successes.

| Run | Outcome | Intake/s | Acceptance p99 ms | Last accept → visible ms | First accept → visible ms | Verified headers / bodies |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| Individual first | PASS | 431.609 | 52.605 | 7,157.593 | 18,715.740 | 5,000 /8 |
| Group16 first | PASS | 321.390 | 286.414 | 251.152 | 15,782.496 | 5,000 /8 |
| Individual repeat | FAILED verification; partial | 416.665 | 56.158 | 6,798.618 | 18,775.787 | 0 /0 |
| Group16 repeat | PASS | 317.101 | 378.261 | 244.297 | 15,982.458 | 5,000 /8 |
| Group4 first | FAILED verification; partial | 443.658 | 54.411 | 389.533 | 11,634.941 | 0 /0 |
| Group4 repeat | FAILED verification; partial | 360.758 | 189.832 | 457.855 | 14,118.152 | 0 /0 |

First-accept-to-visible is calculated directly from each report's
`all_visible_unix_seconds - first_accept_unix_seconds`, not rounded SMTP+lag.
SELECT polling contributes to these end-of-load observations; they are not
per-message publication percentiles. Group16 intake was 24–26% slower across
the samples, with worse acceptance p99, despite shorter visibility tail and
first-accept-to-visible interval. Group4 did not consistently retain the
individual path's acceptance behavior. Shared-resource contention and host
variation remain possible explanations, not proven causes.

The individual repeat and first group4 run returned generic
`NO Header unavailable within prototype parsing limit` during subsequent cold
SEARCH. Neither verified headers/bodies. The group4 repeat, after error
classification was improved, explicitly returned `Header operation timed out`.
That repeat confirms its timeout, not the earlier generic errors' causes. The
SEARCH deadline was 30 seconds. Successful cold/warm SEARCH timings were
25,171.544/190.194ms (individual first), 29,974.609/199.714ms (group16 first), and
25,701.334/209.309ms (group16 repeat). The variability reinforces the need to
avoid causal claims from these single runs. Fixture cleanup completed normally.

These results concern a disposable single-host experiment. They establish no
independent-disk, cross-host, availability-zone, sustained-backlog, production
burst/daily-capacity or general throughput guarantee. The 1KiB preaccepted drain
and 64KiB simultaneous intake workloads have different boundaries and must not
be compared as interchangeable capacity estimates. See [local-load.md](local-load.md)
for harness scope and [acceptance-profile.md](acceptance-profile.md) for the
separate admission-lock investigation.

## Source identities and local artifacts

| Source | Individual baseline | Group16 experiment |
| --- | --- | --- |
| `mailbox_linux.go` | `5b84e9036d18e620f73a0ea2b4cfad336932ef0b25cca9e5800477108376e8f7` | `10898f7deb70742020137f31693b95b5888f6a37f99455d3cefa50f410f3bf07` |
| `schema.sql` | `cef1d12e0444d127ca29ef86fda3a201276783be563ca34cd6a3498f0aed6887` | unchanged |
| Measurement test | `9fde4278e8dbe9a858779b312347dc7404d108e1564d15eaa9c2c15a448f1bd2` | unchanged |
| Wrapper | `2893dfbcc1fdd1c09b1a8e73fe07439e54a281676bdb7949911d7435d07faf66` | `1d6f8b4aede9cdaffcb828085aa7d271a46117f0e2c6d17008a9b5e4adea00ac` |

The wrapper difference is cleanup/report hardening, not a drain-boundary change.
The inspected group4 source was
`15c5029114a349441172af23adf14acebc4700a37c912317ac398ff8ca99e6c2`.
Isolated reports are `/tmp/fry-publication-baseline.json` and
`/tmp/fry-publication-group-1.json`. Simultaneous reports are
`/tmp/fry-load-5000-shared.json`, `-group.json`, `-shared-repeat.json`,
`-group-repeat.json`, `-group4.json`, and `-group4-repeat.json` (all with the
`/tmp/fry-load-5000` prefix). These temporary machine-local artifacts are not
repository fixtures. Historical candidate source and fault tests are preserved
in the unapplied patch linked above; current production remains unbatched.
