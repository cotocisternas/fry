# Header capture during acceptance

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Acceptance now optionally captures bounded original headers while the same bytes
are already streamed to both durable copies. It avoids a later original reread,
read-pin lifecycle and separate metadata commit for captured messages. It does
not require valid email headers or guarantee capture of every valid message.
The existing authenticated lazy `Store.Header` path remains the fallback for
uncaptured messages and older hot or packed receipts.

The collector observes the single broadcast stream, retains only bytes through
the header terminator, and hashes/counts the complete stream. After both replica
writers succeed, a bounded parse validates the captured prefix outside SQL locks.
Finalization compares its complete-stream size/SHA-256 against **both** durable
locators before inserting immutable metadata beside the new receipt, in the
existing acceptance transaction. It adds an INSERT when capture succeeds, not a
new acceptance commit. The metadata has the existing receipt lifetime: scoped
live reads, no independent ownership reference, and guarded deletion on cleanup.

An already accepted retry returns through durable authority before reading the
replacement source; caller bytes cannot replace its header. An interrupted
prepared retry must stream matching complete bytes to obtain usable capture.
Source-free finalization remains valid without metadata. Rollback also removes
the inserted header; an uncertain commit reply is resolved through durable
acceptance status. Once admitted, metadata insertion failure remains a database
failure of acceptance, rather than a silently committed partial transaction.
Malformed, unterminated or oversized headers, or unavailable optional capture/
parse capacity, simply omit metadata; they do not reject otherwise acceptable
mail. Stream/copy errors retain their existing acceptance failure semantics.

Each Store has 16 nonblocking retention permits held through finalization,
including SQL waits. Each prefix uses adaptive allocation with a 1 MiB maximum:
**16 MiB is a retained-prefix capacity bound per Store, not total process RSS**.
Allocation growth, parser copies, hashing, database encoding, and other stores
add memory beyond that figure. A single nonblocking parse permit bounds concurrent
validation; a valid header can be skipped when that permit is occupied. Neither
permit queues optional work or blocks incoming mail waiting for extraction.
There is no background extraction queue, whole-body cache, or search worker
fanout. Optional skipping means cold lazy derivation remains necessary.

Focused regression coverage includes terminators across chunk boundaries,
complete-stream proof, caps and optional parse/retention exhaustion, accepted
retry ignoring replacement bytes, source-free recovery, mismatched durable-copy
proof, interrupted streams, budget release, metadata rollback, insertion failure,
and accepted-but-lost-response atomicity. Existing lazy-header authority and
cleanup tests remain relevant; this feature does not bypass read pins for
fallback originals. Test/review status and the end-to-end result are recorded
when their coordinated runs complete.

## Initial isolated acceptance evidence

The separate untraced acceptance baseline uses 1,024 × 64 KiB originals,
16 workers, a prewarmed 16-connection pool, and one local pair on shared storage.
Both samples verified every receipt and full original after capture stopped and
checked disposable database cleanup. Environment and scope are detailed in
[acceptance-profile.md](acceptance-profile.md).

| Metric | Shared admission, no capture | Shared admission with capture |
| --- | ---: | ---: |
| Accepted / verified full originals | 1,024 / 1,024 | 1,024 / 1,024 |
| Elapsed | 891.577 ms | 927.778 ms |
| Observed accepted originals/s | 1,148.527 | 1,103.712 |
| Acceptance p99 | 23.350 ms | 28.548 ms |
| Metadata receipts immediately after acceptance | not measured | 795 / 1,024 |
| Metadata raw-header bytes | not measured | 52,470 B |
| Receipts requiring fallback if subsequently queried | not measured | 229 |

The observed rate was about 3.9% lower in this single comparison, not a stable
cost bound. Capture coverage was 77.6%; these counters do not identify whether
retention or parse contention caused the skips. Coverage is queried outside the
acceptance timer, runtime profiles and SQL tracer, **before** any content
verification could populate metadata. It does not call `Header` or read bodies.
The report retains the unchanged acceptance capture boundary.

Evidence: `/tmp/fry-acceptance-baseline-capture-1/baseline/report.json`, SHA-256
`d27ebd095531d964b6769152579eb03bd69df8cffffb43c9d23604d05107eb83`.
The final `run.json` confirms cleanup. Relevant recorded source hashes:

- `header_capture_linux.go`: `1bf7c6377982ee5765c35039860fb40dc85d89b0fb34cf0cde17b8e24045bdb4`
- `store_linux.go`: `275f6cd4a3cdc397c257b4add54745198e54ee3b85d08161f28b1f8b5dbafc6e`
- `postgres.go`: `c55d9f1fb6550c3f98c7143a9bc0adc07c36c82fe96d2e6b6c304623bfba6caf`
- Measurement test: `3988365c37e1496713404ae1b36c310b99038e2a8b63f72fee4742d9a9d8bae1`

The capture diagnostic profile also passed all 1,024 receipt/full-body checks
and cleanup: `/tmp/fry-acceptance-profile-capture-1/profile/report.json`.
It measured 2,461.743 ms, 415.966/s and p99 307.633 ms, with 872 captured headers
(57,552 B) and 152 fallback receipts. It recorded 4,096 successful commit events,
the same four per acceptance as the previous diagnostic; successful metadata
capture did not add a separate acceptance commit. Summed commit time was 28.382s
and native pool acquisition time 11.369s, both overlapping across workers, not
an additive wall-time breakdown. Diagnostic elapsed was 2.65 times this feature's
untraced baseline. This large single-pair difference is not attributable solely
to capture or instrumentation; it is a diagnostic result, not the performance
baseline. The matching SMTP/IMAP observation is recorded below. These are local
synthetic samples, not production throughput, burst capacity or independent-
failure-domain evidence. Near-limit headers and sustained workloads may have
different allocation, SQL/WAL and latency costs.

Capture diagnostic artifact SHA-256 identities:

- `profile/block.pprof`: `e8d60051364c9653c09c45411fafd2eea17a27df571c40854fbed7c50a368fda`
- `profile/cpu.pprof`: `51f02f03c2073eedab528b3737a05b4ef0938d94ba90c49390535e164c958b30`
- `profile/mutex.pprof`: `9918670320ddd101aac5411406a8f0af2f70d2d4ae2f1bad8b0cd39ad982ad9f`
- `profile/report.json`: `76d83acd4874ab2704242333d2374c2944764f0f3b75a18f963d6da84d159c76`


## First simultaneous SMTP/IMAP result

`/tmp/fry-load-5000-capture.json` passed the complete workload: 5,000 accepted,
zero SMTP deferrals/failures, 5,000 unique headers/metadata/flags verified and
eight complete-original plus partial-read samples. Both comparison runs used
individual mailbox publication (batching was not adopted), shared normal
admission, 64KiB messages, 16 sequential SMTP workers and one local pair. Other
heavy measurements were paused.

| Metric | Shared admission, no capture | With optional capture |
| --- | ---: | ---: |
| SMTP elapsed / accepted per second | 11.585s /431.609/s | 12.069s /414.296/s |
| Acceptance p99 | 52.605ms | 55.211ms |
| Last acceptance → all-visible | 7,157.593ms | 2,564.570ms |
| First acceptance → all-visible | 18,715.740ms | 14,607.757ms |
| Cold combined header/date/unseen SEARCH | 25,171.544ms | 14,037.707ms |
| Identical warm SEARCH | 190.194ms | 188.632ms |
| Headers / full-and-partial samples verified | 5,000 /8 | 5,000 /8 |
| FRY peak RSS, client/PostgreSQL excluded | 47,748KiB | 41,344KiB |

First-acceptance-to-visible is calculated from report timestamps. The no-capture
report is `/tmp/fry-load-5000-shared.json`. Cold SEARCH was about 44% shorter in
this individual comparison, while observed acceptance rate was about 4% lower.
Visibility improved in this sample, but these runs do not establish that capture
caused the lag difference, a stable throughput cost, or a memory reduction.
The SMTP report does **not** count captured metadata before SEARCH: do not infer
an exact captured or fallback count for these 5,000 messages from the isolated
795/1,024 acceptance fixture. Cold SEARCH still took 14seconds, and optional
fallback remains a material first-access cost.

This is the first complete local end-to-end observation, not production capacity
or a statistical performance guarantee. It verifies all headers but only eight
full originals. `go vet ./...` and the final combined PostgreSQL race suite
(`./scripts/tests/receipts.sh -race -short`) passed with individual mailbox
publication, shared admission, header capture and the clarified IMAP errors.
