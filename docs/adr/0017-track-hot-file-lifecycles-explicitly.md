# Track hot-file lifecycles explicitly

Introduce an authoritative per-member file catalog through the existing single-writable-file rewrite protocol before adding segment rotation. Keep `incarnation` as the immutable physical file identity and retain the existing member identity and process-owner epoch; reinterpretation would break historical write, read and cleanup obligations. Catalog transitions and locator publication share the existing transaction, so recovery can verify exactly which file it may remove instead of inferring authority from directory names; this adds metadata and a fresh-schema requirement but establishes a testable file lifecycle without simultaneously changing concurrency or framing.

See the [bounded implementation design](../design/member-file-catalog.md). This decision does not select a segment size, sealed-file index or automatic crash-takeover mechanism.
