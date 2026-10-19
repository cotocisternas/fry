# Internal member RPC

The private `fry.member.v1.MemberService` contract lives in
[`proto/member/fry/member/v1/member.proto`](../../proto/member/fry/member/v1/member.proto).
Go bindings are under `gen/member/go/fry/member/v1`; TypeScript bindings are under
`gen/member/ts/fry/member/v1`. Reproduce them with the pinned, offline generator:

```sh
scripts/generate/proto.sh generate proto/member gen/member
scripts/generate/proto.sh check proto/member gen/member
```

Connect-Go v2 serves protobuf gRPC over HTTP/2 on the dedicated member listener.
TLS 1.3, CA verification and a configured coordinator leaf SPKI SHA-256 pin are
required. Coordinators verify the server hostname/CA and configured server SPKI
pin. Both sides recheck certificate validity per call, including pooled sessions.
The coordinator also guards every socket write beneath TLS using the verified
chain's intersected validity interval. The guard is armed after handshake and
before HTTP/2 receives the connection; expired pooled connections cannot transmit
request headers or payload. Response validation is an additional check, not the
transmission boundary. No timer or trace callback authorizes writes.
Redirects, proxies and compression are disabled. The listener rejects HTTP/1.1,
Connect JSON and gRPC-Web. Administration listeners do not expose these methods.

## Exact build matching

Every call carries `Fry-Release`; the server requires exact equality with its
build identity before dispatch. Identity responses repeat that identity; dialing
checks it alongside member ID, failure domain, nonzero incarnation and owner epoch.
The build identity is independent of the protobuf API major.

Release packaging must embed the same immutable build identifier in both binaries:

```sh
go build -ldflags='-X=github.com/cotocisternas/fry/internal/receiptstore.release=RELEASE_ID' ./cmd/fry
go build -ldflags='-X=github.com/cotocisternas/fry/internal/receiptstore.release=RELEASE_ID' ./cmd/fry-member
```

`RELEASE_ID` must identify exact source and dependency inputs, such as an immutable
release tag plus source digest, and change whenever that build changes. Without
this override, source builds use `prototype-member-grpc-v1`, the main module
version, and available Go build-info `vcs.revision`/`vcs.modified` values.
Unversioned development/test binaries deliberately share their development
identity. A dirty flag does not fingerprint edits: this fallback is unsuitable
for mixed-build release deployments. The embedded override supplies release
matching authority; callers cannot negotiate a different value through the wire.

## Operations

| RPC | Authority, completion and retries |
| --- | --- |
| `Identity` | Samples live identity under the member gate. Closed/frozen members fail. Refresh accepts only the same member, failure domain and owner epoch. |
| `Write` | Rechecks the existing database claim, organization/receipt/target, lease epoch, profile, incarnation and owner. The existing durable receipt authority publishes the copy before response success. Retry an uncertain result through the same attempt protocol. Wire fields never create authority. |
| `Read` | Resolves the immutable original through receipt metadata and a current parent pin; creates a fresh member read operation/pin; validates the range; records actual closure before completion. Storage/database authority supplies the locator and original identity. |
| `Cancel` | Checks organization/receipt/attempt and aborted state, interrupts input, waits for writer quiescence and records cancellation proof. A disconnect alone supplies no cancellation proof. |
| `Maintenance` | Runs rewrite, explicit rotation, sealed reclaim or conditional automatic rotation with existing operation IDs, gates, owner/incarnation fencing and recovery. `status_only` checks stored operation kind and owner epoch. Retry the same ID after uncertain delivery. |
| `StorageStatus` | Samples diagnostics without waiting on the member gate, retaining a database transaction or refreshing routing. Busy samples leave guarded fields absent. Frozen/closure-debt state stays observable. |

Rewrite and explicit rotation refresh the physical incarnation on success. Sealed
reclaim preserves current-file routing. Automatic rotation uses a nonblocking gate
and returns `complete`, `stale`, `conflict`, `busy`, `capacity` or `uncertain`; only
`complete`/`stale` include refreshed identity. Maintenance status is `running`,
`complete` or `failed`. Timeout never proves closure or rewrites durable outcome.

## Bounded streaming

A write begins with exactly one `Target`. Payload frames contain nonempty chunks
of at most 65,536 bytes, followed by exactly one `Completion` containing total size
and SHA-256, followed by clean stream EOF. The transport normalizes only
Connect v2.0.1’s local outer `CodeUnknown` error with message `EOF`, no details,
and a direct `io.EOF` cause; the
integrity reader then requires the exact EOF sentinel. Wrapped or joined EOF
carrying another failure is rejected, including after a valid completion.
Reads use the same payload/completion
sequence without a target. Aggregate payload is bounded by configured maximum
original size (at most 256 MiB), and reads require the requested range length.
Each protobuf message is capped at 69,632 bytes before decoding. Frame count is
bounded by `limit / 1024 + 1024`, including completion, to constrain fragmentation.

Missing/out-of-order/duplicate frames, empty/oversized chunks, aggregate excess,
size/hash mismatch, trailing data, missing final gRPC status and cancellation all
fail. Readers retain one pending chunk: exact-length consumers cannot observe
final payload bytes before completion and clean EOF. Originals never accumulate
in a single protobuf message or whole-original buffer.

Write completion must validate before local append may publish the copy. The
empty `WriteResponse` follows durable copy publication. Read completion follows
actual storage-handle closure and durable closure proof. Previously delivered
chunks remain partial on error and authorize neither acceptance nor completion.
Closure uncertainty uses `DATA_LOSS` and preserves existing closure debt. This
marker takes precedence over joined Connect errors, cancellation, deadlines,
conflict, stale-owner and busy errors; none may authorize replica fallback or
parent-pin cleanup while descriptor closure remains unproven.

Read registration checks cancellation before sending its database COMMIT. Once
COMMIT is sent, it has a separate five-second settlement budget so request or
ownership cancellation cannot discard a successful registration acknowledgement.
A known committed registration still requires cleanup and closure proof;
genuinely uncertain outcomes retain debt. The [CI recovery record](../validation/ci-recovery.md)
links the regression and tested revision. This does not provide automatic crash
takeover or eliminate uncertainty after an unavailable database.

Calls have the configured 1 second–5 minute budget. Admission bounds active calls
and raw TLS connections; excess calls fail `RESOURCE_EXHAUSTED`. HTTP/2 also
limits concurrent streams per connection to the configured request budget, with
128 KiB per-stream and 1 MiB per-connection receive windows and 4 KiB header tables. Cancellation
closes the affected request body and sets its HTTP/2 stream write deadline to
interrupt blocked protobuf receives/sends without affecting unrelated streams. Shutdown cancels ownership before
waiting for members. Maintenance retains its existing durable recovery semantics.

## Errors and diagnostic client

Peer denial is `PERMISSION_DENIED`; release/owner/incarnation mismatch is
`FAILED_PRECONDITION`; malformed claims/streams are `INVALID_ARGUMENT`; busy
admission is `RESOURCE_EXHAUSTED`; cancellation/deadlines retain standard gRPC
codes. Other storage failures return generic `UNAVAILABLE`, without SQL or private
receipt metadata. An error response never proves an effect did not commit.

Normal callers use `DialRemoteMember`, which supplies pinned mutual TLS, release
metadata and gRPC mode. A generated diagnostic client requires the same trust:

```go
client := connect.NewClient(connecthttp.NewTransport(httpClient, memberURL,
    connecthttp.WithGRPC(), connecthttp.WithReadMaxBytes(69632)))
rpc := memberv1connect.NewMemberServiceClient(client)
ctx, info := connect.NewClientContext(ctx)
info.RequestHeader().Set("Fry-Release", releaseID)
identity, err := rpc.Identity(ctx, &memberv1.IdentityRequest{})
```

## Chunk compression and allocation measurement

A payload frame may alternatively contain `compressed_data`: raw DEFLATE
(RFC 1951) bytes with the exact uncompressed chunk size. Both encoded payload
and declared uncompressed size are capped at 65,536 bytes. The receiver uses
bounded alternating chunk buffers, requires exactly the declared output, a valid
DEFLATE end with the exact EOF sentinel and no trailing encoded bytes. The
inflater reads a private `bytes.Reader`; wrapped or joined EOF is rejected. Aggregate limits and the terminal
SHA-256 continue to address original uncompressed bytes. Malformed compression
never supplies completion or durable acceptance.

Compression state is reused within each stream. Small chunks and chunks whose
compressed representation is no smaller use ordinary `data` frames; peers must
support both frame variants. Compression is at the protobuf chunk level, with
HTTP/gRPC compression still disabled. No whole-original compression buffer is
created.

The existing 200 MiB allocation test uses a repeating generated pattern. Chunk
compression reduces its actual encoding, decoding and HTTP/2/TLS allocation cost.
The unchanged 100 MiB cumulative allocation limit therefore measures this
compressible fixture. It is not a promise of that cumulative budget for an
incompressible 200 MiB original: raw protobuf decoding and transport pools still
allocate per chunk. Bounded message, decompression and aggregate limits apply to
both cases; cumulative allocation and peak live memory are different quantities.
