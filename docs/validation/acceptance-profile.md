# Local acceptance baseline and profiles

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

On 2026-10-17, separate baseline and diagnostic runs each accepted 1,024 synthetic 64 KiB originals through public `Store.Accept`, then verified all 1,024 receipt statuses and original bodies. Both runners checked successful disposal of their PostgreSQL container before publishing `run.json`. This measures isolated local acceptance, not production capacity or SMTP delivery throughput.

## Historical fixture filesystem qualification

The historical `/tmp/fry-acceptance-*` runs below placed build/fixture scratch
under their output directory: `measure-acceptance.sh` exported that location as
`TMPDIR`. The current mount inspection identifies `/tmp` as memory-backed on
this host. Thus these historical hot-member fixtures are inferred to have used
tmpfs from the runner, paths and current mount; their reports did not record
per-run filesystem metadata. They are not evidence of persistent hot-disk fsync
performance. PostgreSQL ran separately in its disposable Docker container.

All recorded values, source hashes and the unexplained outlier remain valid as
reported local observations; this qualification does not attribute that outlier
to filesystem choice. Full SMTP load fixtures used repository-local Btrfs and
remain a separate persistent-filesystem case. Compare measurement boundaries and
fixture placement before comparing their rates.

Reproduce with a new output directory for each run:

```sh
scripts/bench.sh acceptance baseline /tmp/fry-acceptance-baseline-NEW
scripts/bench.sh acceptance profile /tmp/fry-acceptance-profile-NEW
# Optional separate diagnostic; not run for the evidence below:
scripts/bench.sh acceptance trace /tmp/fry-acceptance-trace-NEW
```

The historical opt-in `TestAcceptanceMeasurement` runs below used 16 workers, a prewarmed 16-connection pool, 16 concurrent appends, a 30-second acceptance lease, and one local member pair with 128 MiB capacity per member. The current runner also accepts a third `legacy|bounded` argument (default `legacy`), uses 256 MiB capacity for both profiles, and records the actual file limits. A fourth argument selects the maximum message size (`65536` by default or `1048576`); actual bodies remain 64 KiB, so the latter exercises real reservation refunds. Current bounded reports also verify and record the derived capacity-lane layout after the timed interval. The bounded fixture selects 2,048 records and 96 MiB per file to leave rewrite/staging headroom; these are synthetic fixture sizes, not production recommendations. This later harness change does not alter historical reports. Both members use the same host filesystem. Corpus generation, fixture setup, complete original-byte/hash checks, and receipt/reference/target/work validation occur outside the captured acceptance interval. No SMTP, mailbox publication, or IMAP worker runs in this measurement. The acceptance context is bounded to 180 seconds, Go test to 240 seconds, and the runner's Go command to 300 seconds.

| Measurement | Baseline | Profile + SQL/acquire tracing |
| --- | ---: | ---: |
| Accepted / attempted / verified originals | 1,024 / 1,024 / 1,024 | 1,024 / 1,024 / 1,024 |
| Errors | 0 | 0 |
| Acceptance elapsed | 6.086 s | 6.996 s |
| Accepted originals/s | 168.24 | 146.36 |
| Latency p50 | 90.45 ms | 96.15 ms |
| Latency p95 | 130.75 ms | 190.80 ms |
| Latency p99 | 136.98 ms | 267.56 ms |
| Latency maximum | 144.09 ms | 278.82 ms |
| Pool acquisitions | 8,192 | 8,192 |
| Acquisitions finding an empty pool | 3,124 | 2,955 |
| Summed pool acquisition duration | 3.318 s | 3.695 s |
| New / canceled connections or acquisitions | 0 / 0 | 0 / 0 |
| Go process allocated bytes during interval | 409,407,568 | 456,777,760 |
| Go process GC cycles during interval | 6 | 7 |

The diagnostic interval was 14.95% longer and its measured acceptance rate 13.00% lower. These are two individual runs, not confidence intervals or a stable overhead estimate. Diagnostic overhead includes CPU-profile streaming, sampled block/mutex profiling, and SQL/acquire aggregation together. Baseline installs no SQL tracer and enables no runtime profile or trace; its empty SQL array means tracing was disabled, not that acceptance issued zero queries. Allocation counters cover the Go test process and its background/profiling goroutines, not PostgreSQL or an isolated production server.

The diagnostic captured 4,096 transaction-begin and 4,096 transaction-commit events, 8,192 traced acquisitions, zero SQL errors, and zero histogram overflow. SQL and acquisition durations overlap across workers and cannot be added into a wall-clock breakdown. The largest SQL fingerprint (`40cea2fb29fe8759d8ae04c552c1829d653b19a8259f8f5f3dfbb74959e577c8`, classified `other.statement`) accumulated 95.589 seconds over 2,048 calls; commit accumulated 10.467 seconds over 4,096 calls. Root and two independent source reviews mapped that fingerprint to the exclusive transaction advisory lock in `lockInboxAdmission` (`migration_linux.go`): `pg_advisory_xact_lock(hashtextextended('fry-inbox-admission:' || $1,0))`. This identifies concentrated-inbox admission serialization as a concrete optimization candidate. The aggregate is overlapping time across workers, not 95.589 seconds of wall-clock runtime, and does not by itself quantify the improvement from changing lock mode. The initial results below preceded the shared normal-admission change; the subsequent measured comparison follows.

The environment was Linux/amd64, Go `go1.27.2`, 24 logical CPUs and `GOMAXPROCS=24`, with PostgreSQL `18.6 (Debian 18.6-1.pgdg13+2)`. The disposable PostgreSQL image was pinned to `postgres@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336`. These runs do not establish behavior across hosts, independent disks, networked members, availability zones, sustained queue growth, or peak production traffic. They are not a controlled comparison with the separate SMTP+IMAP or publication measurements.

Actual local evidence directories are `/tmp/fry-acceptance-baseline-1` and `/tmp/fry-acceptance-profile-1`. They are temporary machine-local artifacts, not repository fixtures. Each includes a final `run.json` with checked-cleanup status and artifact sizes/hashes; the mode child contains:

| Artifact | SHA-256 |
| --- | --- |
| `baseline/report.json` | `105a61d4ac703b92d0a7ea42999d4ea56fa4b414a3bc511568f3e4734b071503` |
| `profile/report.json` | `a950fe9e851c9bb693d2f95c0b8468495b7428bf5510602691a0fc53f48a6285` |
| `profile/cpu.pprof` | `aa28f0c80949c3d43c038b626b1fc34480e53b38dfae73cb8440b4de875b7781` |
| `profile/block.pprof` | `bb0cea84cc7f83ea0a0c17e243ceb76df7d0b796e825cf8cb6c4bbb0d8770162` |
| `profile/mutex.pprof` | `52120814c823fe99d327782db6d03d42905ca66a5eb0d7550336b43b81afd5e4` |

Both reports record identical source hashes, including:

| Source | SHA-256 |
| --- | --- |
| `scripts/bench/acceptance.sh` (removed wrapper) | `88aec1cec3cc5f592f278c616cbe96a20ff06718d0d159d5911680845132ca9a` |
| `internal/receiptstore/acceptance_measurement_linux_test.go` | `b12c31c5206965409ab8f77870d36bfa3f5419921c7c251f28c7d555b7f17927` |
| `internal/receiptstore/store_linux.go` | `f2b463ec75586b64bcd4354b62f01ca894f6097626efd586a4bd93eec6b3ce0e` |
| `internal/receiptstore/postgres.go` | `de2bb502828b305a02dd2829433a0837f0dac0b32409efd7927845f2a3fd1fd4` |
| `internal/hotstore/store_linux.go` | `46beff03792da6d185e673bf8d372c8caadf4b31fb1d6be9770b89420f441169` |
| `internal/receiptstore/schema.sql` | `cef1d12e0444d127ca29ef86fda3a201276783be563ca34cd6a3498f0aed6887` |

The separately inspected admission-lock source was `internal/receiptstore/migration_linux.go`, SHA-256 `79fc66a0acc7dbe7f99f10b8fd12439b826c6ea0ce59bdac586c6ab5533982c3`; this file was not included in the measurement report's original source-hash set.

The runner requires a new private output directory and local Unix Docker endpoint, bounds fixture startup and cleanup, and validates the exact artifact set and fixed workload before success. Reports omit SQL text, arguments, DSNs, passwords, and original bodies. A failed run retains only a bounded, sanitized last 64 KiB diagnostic tail as mode-0600 `failure.log`, with the fixture DSN/password replaced; it does not publish successful `run.json`. Lightweight offline fixtures checked success ordering, failed cleanup/test suppression, private permissions, artifact validation, and useful redacted failure tails. No trace run is part of this evidence.


## Shared normal-admission follow-up

After the diagnosis, normal admission was changed to acquire the same advisory key in shared mode; migration reservation retains its exclusive lock. Preparation and finalization still check migration state, and normalized destination acquisition order remains unchanged. Focused real-PostgreSQL tests passed and independent source review approved the change before these follow-up measurements. This is an implemented admission-lock change, not folder UID group commit.

The same fixed workload and capture boundaries were rerun sequentially in fresh disposable fixtures. All 1,024 receipts and raw originals passed verification in each run, with zero acceptance errors, zero SQL overflow, and checked fixture cleanup. The environment, pinned PostgreSQL image, runner, and production hashes other than `migration_linux.go` match the before runs. The test hash changed only to include that critical production file in its source-hash set.

| Measurement | Before exclusive normal admission | After shared normal admission |
| --- | ---: | ---: |
| Baseline elapsed | 6.086 s | 0.892 s |
| Baseline accepted originals/s | 168.24 | 1,148.53 |
| Baseline latency p50 / p95 / p99 | 90.45 / 130.75 / 136.98 ms | 13.44 / 17.89 / 23.35 ms |
| Profile elapsed | 6.996 s | 0.915 s |
| Profile accepted originals/s | 146.36 | 1,119.17 |
| Profile latency p50 / p95 / p99 | 96.15 / 190.80 / 267.56 ms | 13.84 / 18.49 / 21.73 ms |
| Profile admission-lock calls | 2,048 | 2,048 |
| Profile summed admission-lock duration | 95.589 s | 0.104 s |
| Profile summed commit duration / calls | 10.467 s / 4,096 | 8.684 s / 4,096 |
| Baseline summed pool acquisition duration / acquisitions | 3.318 s / 8,192 | 3.596 s / 8,192 |

The after baseline rate was 6.83 times the before baseline in these individual runs. This is evidence of improvement for this concentrated-inbox local workload, not a general capacity ratio or a statistically estimated speedup. After diagnostic elapsed was 2.62% above its separate baseline; short intervals and single samples limit overhead comparisons. Pool waits and commits remain overlapping concurrent totals, not additive fractions of elapsed time. No new throughput bottleneck is established solely by their relative sizes.

The shared SQL fingerprint is `1503f55a794e36b1ed70c950a96a95592a8a7493ee802d78d59a22f21b02ccaa`, computed from the actual whitespace-normalized `pg_advisory_xact_lock_shared` statement in `lockOrdinaryInboxAdmission`. Its exact accumulated duration is 104,104,444 ns across 2,048 calls, compared with 95,589,421,307 ns for the prior exclusive statement. Both are classified `other.statement`; the matching fingerprint and source, rather than that broad label, establish attribution.

Follow-up evidence is in `/tmp/fry-acceptance-baseline-shared-1` and `/tmp/fry-acceptance-profile-shared-1`:

| Artifact/source | SHA-256 |
| --- | --- |
| After `baseline/report.json` | `dfd9fe17ab30951fb4591e8faeab87e5d90d052acdc23fa5d7a23377aec63f76` |
| After `profile/report.json` | `5498dc471190c167e2c57c7eba4435006e22cd002c35cbf6f81f77ddae72348b` |
| After `profile/cpu.pprof` | `6b9e8b19dbe1d024c79e8a237ec48be503722f63c63e2c82a2d5fe0f70907749` |
| After `profile/block.pprof` | `8a3a7b43e1c3c54289ab31ddbc3b472c0e475fdc7d238a532572ee2a16cc6713` |
| After `profile/mutex.pprof` | `aa7bdc6ea3c09168f3bb497c03851c062a0856428e836a309150891aaa53841e` |
| After `migration_linux.go` (in both after reports) | `0a4e496295ebbe8e8a233e4e54638b72f0b46c6504663a0b2298c8579d815088` |
| After measurement test | `efb2b6946df596cb8ff725b75cc0688257fde7f2f806bdb1d41165ff9af8c680` |

No SMTP, publication, IMAP, cross-host, sustained-load, or production-capacity conclusion follows from this isolated acceptance comparison. Separate end-to-end load evidence belongs in [local-load.md](local-load.md).

## Optional streaming header capture measurement

The acceptance test now records `captured_header_receipts`,
`captured_header_bytes`, and `uncaptured_header_receipts` using an organization-
scoped metadata count/byte query after the acceptance timer, SQL tracer and
runtime capture have stopped, before content verification. It never invokes
`Header` to obtain these counters, so it cannot lazily populate its own evidence.
There is no minimum coverage assertion because capture is optional. The source
hash set now includes `header_capture_linux.go` when present.

The first capture-enabled untraced baseline at
`/tmp/fry-acceptance-baseline-capture-1/baseline/report.json` passed all 1,024
receipt/full-body verifications and checked cleanup. It measured 927.778 ms,
1,103.712/s and p99 28.548 ms, versus the earlier no-capture shared-admission
baseline's 891.577 ms, 1,148.527/s and p99 23.350 ms. Capture persisted 795 headers
(52,470 raw bytes); 229 receipts had no metadata. The roughly 3.9% rate reduction
is a single observed comparison, not a stable overhead bound. [header-capture.md](header-capture.md) records
implementation, authority, resource limits and the capture source identities.


The capture-enabled diagnostic run at
`/tmp/fry-acceptance-profile-capture-1/profile/report.json` also passed all 1,024
original/receipt checks and cleanup. It measured 2,461.743ms, 415.966/s, p99
307.633ms and 872 captured headers (57,552B), with 152 uncaptured. Its 4,096 commit
events (zero errors) match the previous diagnostic's four per acceptance,
showing capture uses the existing finalization commit. Commit 28.382s and pool
acquisition 11.369s are overlapping aggregate durations. Diagnostic elapsed was
2.65 times the capture untraced baseline, versus the earlier shared-no-capture
diagnostic's 914.967ms. These separate single samples do not establish whether
host variation, instrumentation or another effect explains that difference;
the diagnostic must not replace the untraced performance baseline. The first complete matching SMTP/IMAP observation is now recorded in
[header-capture.md](header-capture.md#first-simultaneous-smtpimap-result):
5,000 headers and eight full/partial originals verified, 414.296/s acceptance
and 14,037.707ms cold SEARCH. That run has no pre-SEARCH capture count; the
isolated fixture coverage must not be extrapolated to it.


The subsequent [storage-accounting measurements](storage-quotas.md#isolated-acceptance-measurements)
retain an unexplained slow first baseline alongside an unchanged-code repeat
and diagnostic. They do not establish a reproducible regression, fixed bug,
or stable overhead bound; see that evidence rather than combining diagnostic
and baseline rates as interchangeable samples.

The same accounting source also passed the complete bounded SMTP/IMAP workload:
5,000 accepted, zero deferrals/failures, all 5,000 headers and eight full/partial
original samples verified. The [quota evidence](storage-quotas.md#complete-bounded-smtpimap-observation)
records 445.656/s and the exact report hash; it remains a distinct boundary from
isolated acceptance profiling, with no production-capacity or overhead bound.
