# Mail storage alternatives

Verified 2026-10-17. These are candidates for FRY's AWS deployment, not selected implementations or measured performance results. The requirement remains durable accepted mail across one Availability Zone loss; cache performance and authoritative storage durability are separate decisions.

## S3 Standard with a discardable body cache

Keep immutable originals in S3 and IMAP headers, flags, dates, UIDs, and body locations in the database. Serve metadata-only commands without fetching bodies; cache recently ingested/fetched bodies on bounded local storage. Cache loss must only cause a refetch. This can reduce repeated body-read latency and GETs, but does not accelerate the first authoritative S3 write or eliminate internet transfer to external consumers. Cache usefulness depends on actual repeat-read frequency.

S3 supports concurrent requests and increasing throughput; published performance guidance is not evidence that FRY's burst is already supported. Measure SMTP acceptance latency separately from IMAP metadata and body-fetch latency. A cache is useful only where a measured bottleneck or latency target justifies its cost. [AWS performance guidance](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance.html)

## Replicated ingest journal, then packed S3 segments

Design hypothesis: first append complete messages to an established replicated journal spanning AZs, then asynchronously seal immutable S3 segments containing multiple messages. Maintain receipt-to-segment, byte-offset, length, and checksum indexes. A final SMTP `250` must wait for the required cross-AZ journal durability **and a recoverable receipt/ownership/work commit**. An in-memory queue or a local journal followed by asynchronous replication does not satisfy this condition. Journal and database are separate systems: crash recovery must reconcile ambiguous writes and commits; a naive dual write is insufficient.

Packing can amortize S3 PUT requests across messages and remove S3 upload completion from the acceptance path. It adds replicated disk capacity, cross-AZ traffic, recovery, and compaction work. Keep journal data until both the segment upload and durable index publication are verified. Readers must handle messages still in the journal as well as those already in S3.

S3 supports byte-range GET, but not multiple disjoint ranges in one GET. Random reads of individual messages will therefore generally still require one GET each. Fetching an entire segment can improve locality but overfetches unrelated bytes. Independently compressed records or seekable blocks preserve selective reads; compressing one whole segment indiscriminately complicates access. [GetObject API](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html)

Per-message expiration and live-mailbox references make deletion harder: tombstone individual records, reclaim a fully dead segment, or copy surviving records into a new segment before retiring the old one. Compaction needs extra space, requests, and atomic index transitions. Segment grouping by retention policy can reduce waste but cannot assume every message expires together. This is additional storage-engine development, not a transparent S3 setting.

## SeaweedFS or Ceph RGW on operator-managed disks

SeaweedFS documents volume replication across servers, racks, and data centers; its write path waits for every configured replica. Its filer separately persists file metadata. Consequently, an S3-facing deployment needs durable metadata as well as replicated content, correct mapping from placement labels to AZs, and master/control-plane recovery. The wiki also describes explicit repair procedures for missing replicas. Validate the selected release's fsync, partial-write, restart, and metadata-commit behavior before interpreting an HTTP success as FRY's required durable receipt. [Replication](https://github.com/seaweedfs/seaweedfs/wiki/Replication), [filer stores](https://github.com/seaweedfs/seaweedfs/wiki/Filer-Stores)

Ceph RGW uses multiple pools, including bucket indexes and data. CRUSH controls failure-domain placement; pool `size` specifies replicas and `min_size` gates I/O when replicas are unavailable. Configure all necessary pools and cluster quorum across the intended AZ boundaries, not just message-data replicas. These mechanisms warrant a prototype, but do not prove five-minute recovery or FRY throughput. [RGW pools](https://docs.ceph.com/en/latest/radosgw/pools/), [CRUSH placement](https://docs.ceph.com/en/latest/rados/operations/crush-map/), [pool settings](https://docs.ceph.com/en/latest/rados/operations/pools/)

Both candidates transfer disk provisioning, spare capacity, repair bandwidth, upgrades, monitoring, and recovery responsibility to the operator. Compare total replicated capacity and operating cost; do not assume eliminating AWS S3 request charges makes either cheaper.

## Storage that is not an equivalent sole copy

An EBS volume attaches within one AZ; multi-Attach does not turn it into cross-AZ storage. S3 Express One Zone also stores data within one AZ. Either can support a cache or a component of an explicitly replicated design, but neither alone satisfies the accepted-mail AZ-loss requirement. [EBS volumes](https://docs.aws.amazon.com/ebs/latest/userguide/ebs-volumes.html), [Express One Zone](https://docs.aws.amazon.com/AmazonS3/latest/userguide/directory-bucket-high-performance.html)

Next evidence: measured body reuse, peak bytes/second, acceptance-latency target, outage/replay tests, and full storage/network/operations cost. The existing São Paulo estimate is a scenario, not workload measurement; changing the object store alone does not eliminate consumer internet egress.
