# S3 key layout and throughput for FRY

Verified 2026-10-17 against AWS documentation. Scope: general purpose S3 buckets and the proposed raw-message key layout; no benchmark or capacity reservation was performed.

## What AWS promises—and what a prefix means

AWS documents **at least 3,500 PUT/COPY/POST/DELETE or 5,500 GET/HEAD requests per second per partitioned prefix**. These are published performance levels, not hard maximums. The write figure covers the grouped operations; it is not 3,500 for each verb independently. AWS supports spreading work over multiple prefixes, but explicitly says scaling is gradual, workload-dependent, and can temporarily produce `503 Slow Down`. Multiplying the published rate by a chosen prefix count is therefore not a guaranteed cold-start capacity calculation. [S3 performance guidance](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance.html)

A prefix is the beginning of an object-key string, not a directory or a provisioned resource. S3 has a flat object namespace; slash-delimited “folders” are a naming/listing convention. AWS explains that internal partitions can be created at any position in a prefix string according to sustained request rate. Consequently, `originals/v1/7f/` is a useful application grouping, not a promise of one independently allocated partition. Writing folder-marker objects does not reserve throughput. [Prefix semantics](https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-prefixes.html), [object-key structure](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-keys.html), [AWS partition explanation](https://repost.aws/knowledge-center/s3-prefix-nested-folders-difference)

Randomized prefixes are **not mandatory**: AWS explicitly retired that recommendation in July 2018 and supports logical/sequential naming. Hash stripes can still be a deliberate FRY distribution choice, rather than a prerequisite imposed by modern S3. [AWS announcement](https://aws.amazon.com/about-aws/whats-new/2018/07/amazon-s3-announces-increased-request-rate-performance/)

## Assessment of the proposed layout

Candidate, not a proven capacity configuration:

```text
stripe = first byte of SHA-256(canonical receipt ID), encoded as two hex digits
key    = originals/v1/<stripe>/<receipt-id>.eml
example: originals/v1/7f/<receipt-id>.eml
```

This creates 256 stable application stripes. Generate a random receipt ID before uploading; retries belonging to the same receipt reuse its key. Hashing the ID avoids concentrating all messages for one large inbox under one inbox-specific naming branch. It also avoids accidental concentration if receipt-ID formatting later becomes time ordered. Keep the algorithm/version stable and record the exact object key in metadata.

This hashes the **receipt ID, not the email body**. Key derivation therefore does not require reading or buffering the entire message. Streaming upload remains possible; SDK multipart buffers, integrity checksums, and replay after network failures require separate bounded-memory decisions. The scheme provides neither content deduplication nor protection against overwriting by itself.

The 256 stripes are not 256 guaranteed internal partitions. Nor do they solve many consumers repeatedly reading one object: that key still belongs to one stripe. Cache/reuse decisions for such traffic need their own measurements. A hot mailbox's database writes and active IMAP owner also remain independent bottlenecks.

## Isolation and request accounting

Stripes organize names; they do not establish tenant authorization. FRY must enforce organization ownership and explicit IAM/bucket access rules. Random IDs are not an access-control mechanism. Separate buckets permit independent bucket settings such as default encryption and versioning; prefixes do not. Neither a naming branch nor an additional bucket establishes an independent regional failure domain. [AWS bucket patterns](https://docs.aws.amazon.com/AmazonS3/latest/userguide/common-bucket-patterns.html)

Capacity planning must count API attempts, including retries, reads, HEAD checks, retention deletes, and maintenance. Multipart uploads require initiation, one request per part, and completion, rather than one request per email; incomplete parts require cleanup. Large-message concurrency also consumes bandwidth and uploader memory. [Multipart process](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpuoverview.html)

If SSE-KMS is selected, KMS has separate request quotas and throttling, varying by operation, region, and key type. Increasing stripe count does not increase those quotas. [KMS request quotas](https://docs.aws.amazon.com/kms/latest/developerguide/requests-per-second.html)

## Verification still required

Test a cold burst and gradual ramp, including the 500,000-message single-inbox batch, concurrent consumption, and retention cleanup. Measure attempted requests by operation, bytes, latency percentiles, retries/503s, connection concurrency, and KMS throttling if applicable. Use bounded retries/backoff and ingress backpressure. The missing burst duration and measured message-size distribution prevent a meaningful throughput target today. AWS recommends monitoring 503s and parallel connections. [Performance guidelines](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance-guidelines.html)

The partition explanation was available through AWS's indexed Knowledge Center content; direct retrieval returned HTTP 403. Internal partition placement and FRY throughput remain unobserved.
