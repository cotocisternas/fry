# Local concentrated-inbox load baseline

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

This is a reproducible synthetic local baseline using the actual `fry` executable, configurable local replica pairs (one pair by default), an isolated PostgreSQL container, and two recipient aliases feeding one inbox. It is not a production benchmark or evidence that FRY sustains 20 million messages/day, a 500,000-message burst, six hours of storage capacity, or availability-zone failure. The user's peak arrival window and deployment hardware remain unspecified.

## Run

From the repository root, with Go, Python 3, Docker, and `stat` available:

```bash
./scripts/bench.sh load-mail
# count, raw bytes/message, sequential SMTP workers, JSON output, optional replica pairs
./scripts/bench.sh load-mail 32 65536 16 /tmp/fry-load-32.json
./scripts/bench.sh load-mail 1000 65536 16 /tmp/fry-load-1000.json
```

Defaults are 1,000 messages, 64 KiB raw messages, 16 concurrent workers, and one replica pair. An optional fifth argument selects 1, 2, or 4 pairs; the fixture divides the same aggregate hot-store capacity budget across them and records accepted attempts per shard. See [static ingestion shards](static-ingestion-shards.md). Limits are 50,000 messages, 1 MiB/message, 128 workers, and 1 GiB total input. Messages must be at least 1 KiB. These limits bound the experiment; they do not promise that every maximum combination finishes within the deadline. The client has a 115-second whole-run deadline, bounded dial/transaction deadlines, and cancellation that closes active sockets. The script additionally bounds fixture readiness and process shutdown; building images/binaries and startup are outside the client measurement.

The script builds both binaries, creates a disposable PostgreSQL 18 container pinned by digest, generates its configuration and local directories, initializes a fresh schema, chooses loopback ports, starts FRY, runs the workload, and removes all fixture state. It never connects to an existing database and configures no outbound relay. The report remains at the requested destination; by default it is `.fry-load-report.json` in the repository root. Credentials are synthetic fixture-only environment values and are omitted from reports.

The standalone command is available for a synthetic fixture you provision yourself:

```bash
go build -o /tmp/fry-mailload ./internal/testutil/cmd/mailload
FRY_LOAD_IMAP_PASSWORD=synthetic-fixture-password /tmp/fry-mailload \
  -smtp 127.0.0.1:2525 -imap 127.0.0.1:1143 -count 1000 -bytes 65536 -concurrency 16
```

It rejects DNS hostnames, wildcard binds, nonloopback addresses, and invalid port numbers. Use an empty synthetic inbox: unexpected visible mail makes verification fail. The harness uses plaintext only on numeric loopback, not public listeners or TLS overhead. Supplying a loopback endpoint still means the operator must ensure it is a disposable fixture.

## What is measured and checked

Each worker reuses a single SMTP connection and sends its transactions sequentially. The recipient alternates between `invoices@example.test` and `billing@example.test`; both map to the same inbox. Every exact-size message contains a run identifier, unique index, deterministic body, body SHA-256 header, fixed RFC 5322 date, and SMTP-safe line lengths. The client does not retry ambiguous failures or deferred messages, so it does not turn lost acknowledgments into duplicate submissions.

JSON records observed SMTP acceptance, 4xx deferrals, other failures, elapsed send time, accepted messages/second, and p50/p95/p99 successful transaction latency. Latency includes client message construction, connection setup when necessary, SMTP commands, streaming, and the final DATA reply. A transport failure is counted as failed/unknown, not proof that the server did not accept the message. Nonzero deferrals/failures or verification failure give a nonzero exit status while preserving the report when available.

After sending, the client polls SELECT until the concentrated inbox contains exactly the observed accepted count. Publication fields include the first/last DATA acceptance times, the all-visible observation, waiting time after SMTP completes, and the observed delay from last acceptance to all-visible. This measures end-of-load visibility, not per-message publication percentiles; login, polling, and SELECT contribute to the observation delay.

It then records another SELECT, two identical UID SEARCH commands combining run-specific header/date/unseen predicates, and a Seen-only UID SEARCH after setting one message Seen. The first combined query is labeled `cold` and the second `warm`; these labels mean first/repeated application query, not flushed operating-system, database, or disk caches. The original `uid_search_header_date_unseen_ms` field remains an alias of the first timing. Exact UID results must agree between the two queries. A streaming header-only FETCH verifies every accepted message index exactly once, its expected hash/date/subject headers, size, internal date, and initial flags. It retains one header response at a time. Up to eight spread-out UIDs receive full PEEK FETCH validation of raw identity, body hash, size, nonzero internal date, and initial flags; each also receives a checked 97-byte partial FETCH. Header/date SEARCH count must equal accepted count, and Seen SEARCH must return exactly the modified UID. Full-payload FETCH sampling is bounded; the harness does not claim to hash every stored payload. All-message header verification and sampled body verification are separate checks.

Client allocated bytes are Go `TotalAlloc` delta over sending and verification, not live heap. Client peak RSS is the operating system's `VmHWM`; the script separately samples the actual FRY process's `VmHWM`. Neither number includes PostgreSQL. Metadata includes Go version, OS/architecture, logical CPU count, GOMAXPROCS, CPU model, memory total, filesystem type, container image, and process scope, without hostnames, passwords, or connection strings. This baseline uses local replicas on one machine and does not model independent failure domains.

## Measured sample: 2026-10-17

Both commands above were run against fresh disposable fixtures. Hardware metadata reported Intel Core Ultra 9 285K, 24 logical CPUs, roughly 62.2 GiB memory, Linux/amd64, Go 1.27.2, GOMAXPROCS 24, and btrfs. PostgreSQL reported 18.6. These are individual runs on a shared workstation, not controlled statistical comparisons.

| Metric | 32 messages | 1,000 messages |
| --- | ---: | ---: |
| Message bytes / workers | 65,536 / 16 | 65,536 / 16 |
| Accepted / deferred / failed | 32 / 0 / 0 | 1,000 / 0 / 0 |
| SMTP elapsed | 0.219 s | 6.436 s |
| Observed accept rate | 145.9/s | 155.4/s |
| Accept p50 / p95 / p99 | 97.1 / 120.4 / 125.9 ms | 97.8 / 145.3 / 154.6 ms |
| Wait after SMTP for all-visible | 183.3 ms | 95.8 ms |
| Last accept to all-visible observation | 242.4 ms | 154.9 ms |
| SELECT | 3.59 ms | 2.39 ms |
| UID SEARCH header/date/unseen, cold | 137.2 ms | 3,335.9 ms |
| Identical UID SEARCH, warm | not measured in this run | 3,235.4 ms |
| UID SEARCH Seen | 3.39 ms | 4.91 ms |
| All-message header/metadata FETCH | 105.4 ms | 3,691.7 ms |
| Unique message headers verified | 32 | 1,000 |
| Full + partial FETCH samples verified | 8 | 8 |
| Client total allocated | 17,456,168 B | 415,149,184 B |
| Client peak RSS | 20,024 KiB | 21,744 KiB |
| FRY peak RSS | 31,812 KiB | 33,980 KiB |

Raw sample reports were generated at `/tmp/fry-load-32.json` and `/tmp/fry-load-1000.json`; rerun the commands to capture reports on another machine. The 32-message report predates the added second-query metric; the final 1,000-message rerun measures both. The warm combined query remained close to the first, while SELECT and Seen-only SEARCH stayed small. Read-only inspection found that [Search](../../internal/imapserver/search.go) loops over mailbox messages and calls [headers](../../internal/imapserver/fetch.go), which opens receipt content and parses headers for each candidate. The inference is that repeated per-message authority/content work warrants profiling; the result does not support attributing the first-query cost solely to lazy MIME index population. It does not isolate which combined predicate dominates, establish an asymptotic bound, or justify extrapolating throughput to larger peaks. No performance tuning is included in this harness change.

## Repeated header SEARCH improvement

A deterministic regression at the real IMAP Search seam counted receipt read pins: searching eight messages twice across authenticated sessions originally opened content eight times per query (counter 8 → 16). The persisted-header implementation leaves the counter at eight for the second query, including after restarting the IMAP server. Membership, flags, and organization authorization still come from the current mailbox operation.

`Store.Header` now lazily derives and stores only the original bounded header, up to 1 MiB per receipt. A warm read obtains its bytes and organization/receipt/reference liveness in one database statement. The API accepts no caller-supplied header. Cold derivation reads the authenticated retained original, validates the same header boundary/parser limit, and commits metadata while holding the receipt guard. Malformed or oversized headers and failed reads are not persisted; MIME search decoding still happens on fresh parsed maps for each query. Database failures propagate rather than silently weakening verification.

This adds bounded-per-receipt header bytes to PostgreSQL; it does not store full bodies or construct a general MIME/search index. There is no server-wide or session header cache in memory, no cache sweep goroutine, and no separate metadata ownership reference. Cleanup deletes the metadata row in the receipt-guarded transaction before committing the removed state. Tests cover population waiting behind cleanup, stale snapshots after actual hot reclamation/removal, expired unswept references, organization isolation, immutable rows, malformed/trailing-body input, parser limits, decoding isolation, and retained-original access after metadata-insert failure.

A fresh single-pair run of `./scripts/bench.sh load-mail 1000 65536 16 /tmp/fry-load-1000-persisted-header.json` produced:

| Metric | Previous 1,000-message baseline | Persisted headers |
| --- | ---: | ---: |
| Accepted / deferred / failed | 1,000 / 0 / 0 | 1,000 / 0 / 0 |
| Unique headers / full-body samples verified | 1,000 / 8 | 1,000 / 8 |
| Combined header/date/unseen SEARCH, cold | 3,335.9 ms | 4,940.4 ms |
| Identical SEARCH, warm | 3,235.4 ms | 58.0 ms |
| All-message header/metadata FETCH | 3,691.7 ms | 65.5 ms |
| SELECT | 2.39 ms | 3.35 ms |
| SMTP elapsed / observed acceptance rate | 6.436 s / 155.4/s | 6.373 s / 156.9/s |
| FRY peak RSS | 33,980 KiB | 35,256 KiB |

The measured repeated query improved by roughly 56× and still returned exactly the same UIDs; the first query did not improve. It now includes lazy metadata population, and its additional writes plus uncontrolled workstation conditions are part of this result. Cold high-volume header search remains unresolved. Each warm candidate still performs a database lookup; batch metadata reads or population during another lifecycle stage would require separate design and measurement. An intermediate untracked RAM-cache experiment was rejected during lifecycle review and is not the implementation or result reported here. These are local, individual measurements with the earlier capacity limitations unchanged.

The combined PostgreSQL race suite (`./scripts/tests/receipts.sh -race -short`) and separate-process smoke (`./scripts/smoke.sh replicas`) also passed with static shards and persisted headers. The [pair comparison](static-ingestion-shards.md#local-pair-comparison-2026-10-17) records the sequential one/two/four-pair measurements. A separate one-message/four-pair harness check verified that reporting includes zero-acceptance shards.

## Larger concentrated-inbox run

After mailbox organization turns and cleanup scan changes, a fresh local run on
2026-10-17 passed with:

```sh
./scripts/bench.sh load-mail 5000 65536 16 /tmp/fry-load-5000-baseline.json 1
```

The collaborating agents paused other heavy tests and measurements during this
run. The hardware, Go/PostgreSQL versions and shared btrfs storage matched the
earlier workstation description. Publication ran in the same coordinator and
used the same 16-connection database pool as incoming acceptance. Lifecycle
maintenance was disabled in this fixture.

| Metric | 5,000 messages |
| --- | ---: |
| Message bytes / workers / replica pairs | 65,536 / 16 / 1 |
| Accepted / deferred / failed | 5,000 / 0 / 0 |
| SMTP elapsed / observed acceptance rate | 41.088 s / 121.69/s |
| Accept p50 / p95 / p99 | 94.0 / 234.4 / 905.8 ms |
| Wait after SMTP for all-visible | 99.6 ms |
| Last accept to all-visible observation | 159.0 ms |
| SELECT | 4.51 ms |
| Combined header/date/unseen SEARCH, cold / warm | 25,014.0 / 251.5 ms |
| All-message header/metadata FETCH | 313.2 ms |
| Seen-only SEARCH | 12.01 ms |
| Headers / complete-body and partial-body samples verified | 5,000 / 8 |
| Client total allocated across send and verification | 2,056,785,024 B |
| FRY peak RSS, excluding PostgreSQL and client | 40,408 KiB |

Every message's header identity, metadata and flags were checked, alongside eight
exact full/partial original samples. This workload does not reread all 5,000 full
bodies. All accepted attempts belonged to the configured single shard. The
allocated-byte figure is cumulative client allocation, not live memory or FRY
allocation. Acceptance timings include client protocol work and the final SMTP
reply; they do not isolate storage fsync or database latency.

Publication caught up promptly in this initial sample. Subsequent isolated
[acceptance profiling](acceptance-profile.md) identified exclusive inbox-admission
advisory locking as a concentrated-inbox serialization point. Normal admission
was changed to share that lock while migration reservation remains exclusive.
The full PostgreSQL race suite passed after this change. The following fresh
5,000-message run used the same workload and fixture shape, with other heavy
measurements paused:

```sh
./scripts/bench.sh load-mail 5000 65536 16 /tmp/fry-load-5000-shared.json 1
```

| Metric | Before: exclusive normal admission | After: shared normal admission |
| --- | ---: | ---: |
| Accepted / deferred / failed | 5,000 / 0 / 0 | 5,000 / 0 / 0 |
| SMTP elapsed | 41.088 s | 11.585 s |
| Observed acceptance rate | 121.689/s | 431.609/s |
| Accept p50 / p95 / p99 | 93.999 / 234.365 / 905.812 ms | 36.886 / 45.607 / 52.605 ms |
| Wait after SMTP for all-visible | 99.611 ms | 7,097.022 ms |
| Last accept to all-visible observation | 158.969 ms | 7,157.593 ms |
| SELECT | 4.506 ms | 9.261 ms |
| Combined header/date/unseen SEARCH, cold | 25,013.997 ms | 25,171.544 ms |
| Identical SEARCH, warm | 251.468 ms | 190.194 ms |
| All-message header/metadata FETCH | 313.195 ms | 282.765 ms |
| Seen-only SEARCH | 12.008 ms | 10.800 ms |
| Headers / complete-body and partial-body samples verified | 5,000 / 8 | 5,000 / 8 |
| Client cumulative allocated bytes | 2,056,785,024 B | 2,059,480,344 B |
| FRY peak RSS, excluding PostgreSQL and client | 40,408 KiB | 47,748 KiB |

The observed acceptance rate increased about 3.55 times in these individual
local runs, while the end-of-load visibility delay increased from 0.159 to
7.158 seconds. Faster incoming acceptance exposed a mailbox publication backlog
in this workload. This is a concrete reason to measure publication separately;
it does not establish per-message publication latency or prove that every
second of the observed delay is spent committing mailbox work. The same
coordinator and 16-connection pool serve acceptance and publication, and SELECT
polling contributes to the visibility observation.

Both reports verified all 5,000 unique headers and metadata/flags, eight exact
full-original samples, and their partial reads. Neither report verifies all
5,000 full bodies. Both used 64 KiB messages, 16 sequential SMTP workers, one
replica pair on shared btrfs, the same pinned PostgreSQL image, and the same
workstation environment described above. This is a measured before/after
comparison, not a statistically established speedup or a production-capacity
claim. It remains far short of validating a 500,000-message burst or production
daily volume. Cold header SEARCH remains a substantial first-access cost.

These results precede mailbox group-commit changes. Any subsequent group-commit
comparison must retain this exact workload, verification scope, and timing
boundaries, and report acceptance together with last-accept-to-visible delay.
The separate [preaccepted publication measurement](mailbox-publication.md)
is useful for isolating the drain step but cannot replace this simultaneous
SMTP/publication observation. No further benchmark is implied by this document.


## Publication grouping experiment not adopted

A subsequent bounded publication-group experiment improved drain visibility in
some runs but worsened intake in the group16 repeats; group4 intake was variable
and both group4 runs failed subsequent header verification. Production retained
individual publication because incoming acceptance takes priority and temporary
publication backlog is permitted. See the [six-run evidence and unapplied
experiment](mailbox-publication.md#historical-bounded-group-experiment-not-adopted).
The final group4 repeat explicitly reported `Header operation timed out` during
cold SEARCH, with zero headers/body samples verified. Earlier generic header
failures remain unattributed; the later timeout does not establish their cause.
Partial SMTP intake results from failed verification runs are not full successes.


## Optional streaming header capture: first complete result

With shared normal admission and individual publication retained, optional
bounded header capture during the existing original stream produced a complete
PASS at `/tmp/fry-load-5000-capture.json`. The same 5,000×64KiB/16-worker/one-pair
workload accepted all 5,000 with zero SMTP deferrals/failures and verified all
5,000 headers plus eight complete-original/partial-read samples.

| Metric | Shared admission, no capture | Optional streaming capture |
| --- | ---: | ---: |
| SMTP elapsed / acceptance rate | 11.585s /431.609/s | 12.069s /414.296/s |
| Acceptance p99 | 52.605ms | 55.211ms |
| Last accept → all-visible | 7,157.593ms | 2,564.570ms |
| First accept → all-visible, direct timestamps | 18,715.740ms | 14,607.757ms |
| Cold combined header/date/unseen SEARCH | 25,171.544ms | 14,037.707ms |
| Identical warm SEARCH | 190.194ms | 188.632ms |
| All-message header/metadata FETCH | 282.765ms | 321.459ms |

Cold SEARCH improved in this single sample, while acceptance rate was modestly
lower. The visibility change is observed, not causally attributed. These runs
do not establish a stable overhead or production capacity. Capture is optional;
the SMTP report has no pre-SEARCH capture-coverage counter, so the isolated
acceptance fixture's capture fraction cannot be applied to these 5,000 messages.
Cold SEARCH still incurs substantial first-access work. See
[header-capture.md](header-capture.md) for authority, bounded retention/parse
fallback, source hashes and isolated acceptance/profile evidence.


## Explicit-rotation implementation follow-up

After integrating authoritative current/sealed reads, the unchanged 5,000×64 KiB, 16-worker ordinary workload passed with 5,000 headers and eight complete/partial samples verified. It observed 429.912 accepted/s, p99 53.922 ms, 7,099.474 ms visibility lag and 8,541.353/239.286 ms cold/warm SEARCH on Btrfs. [The rotation validation record](hot-file-rotation.md#ordinary-smtpimap-follow-up) preserves the report hash, full comparison and limits. This run uses legacy uncapped files; sealed-file correctness is covered separately by actual rotation and restore tests.
