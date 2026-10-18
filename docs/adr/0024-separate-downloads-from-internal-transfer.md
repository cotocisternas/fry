---
status: accepted
---

# Separate file downloads from internal RPC transfer

FRY will use RPC for administration and metadata, authenticated binary HTTPS
streams for browser/CLI downloads of original emails, and bounded protobuf chunks
over gRPC with mutual TLS for internal storage transfers. This gives browsers and
download tools ordinary file-transfer behavior while retaining typed internal
RPC contracts, without placing an entire large original into one RPC message.

Accepted during design on 2026-10-18. Both download and RPC adapters must use the
same application content permissions and auditing. Downloads pass through FRY;
the choice does not expose packed storage objects directly. Internal completion
must preserve the storage protocol's durability and integrity guarantees.

RPC streaming for downloads was viable but not selected. The user subsequently
selected resumable downloads for the initial release: each resumed request must
recheck access and verify the same immutable original. Limits and detailed
transfer semantics remain open in the [API design](../design/api-contract.md).
This decision does not authorize implementation.
