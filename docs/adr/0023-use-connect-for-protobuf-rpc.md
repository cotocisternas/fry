---
status: accepted
---

# Use Connect for protobuf RPC

FRY will use Connect-Go for its protobuf RPC handlers, Connect-Web for the
administration UI, and the gRPC protocol over mutual TLS for internal calls.
The user confirmed that RPC-style HTTP/JSON satisfies administration consumers;
Connect provides that interface and standard gRPC interoperability without an
additional REST translation layer, so grpc-go plus gRPC-Gateway was not selected.

Accepted during design on 2026-10-18. This selects the framework, not permission
to implement it. Email transfer shape is recorded in
[ADR 0024](0024-separate-downloads-from-internal-transfer.md); contract conventions
and exact compatible stable versions remain separate design work. If resource-style REST becomes required,
a gateway can be reconsidered. See the [comparison](../research/api-transport-options.md)
and [API contract design](../design/api-contract.md).
