# API transport options

Investigated 2026-10-18 against official documentation and source. Research informed the accepted [Connect decision](../adr/0023-use-connect-for-protobuf-rpc.md). Implementation was paused during this investigation; no implementation, installation, generation, builds, or performance measurements were performed as part of this research. Scope: protobuf administration APIs for the CLI and browser, plus authenticated member streaming. The user confirmed that RPC-style HTTP JSON is sufficient; REST resource URLs are not required.

Connect administration and member gRPC were subsequently implemented with pinned
native generators. Use the [administration](../reference/admin-rpc.md) and
[member](../reference/member-rpc.md) references for current behavior and
unimplemented mechanisms; this comparison remains the original research record.

## What each option provides

| Concern | Connect-Go + Connect-Web | grpc-go + grpc-gateway |
| --- | --- | --- |
| Administration and CLI | Generated protobuf clients; unary HTTP JSON can also be called with curl. | Generated gRPC clients; gateway supplies HTTP JSON routes. |
| Browser | Connect-Web calls Connect or gRPC-Web directly against the Go server. | Browser uses the gateway's HTTP JSON interface; a separate gRPC-Web solution is needed if that protocol is desired. |
| Server protocols | The same service implementation and HTTP handler stack accepts Connect, gRPC, and gRPC-Web by default. | grpc-go serves gRPC; generated gateway handlers provide HTTP JSON translation. |
| Resource URLs | Connect's native contract uses `/package.Service/Method`; it does not automatically interpret `google.api.http` as resource routes. | `google.api.http` annotations or supported external configuration specify HTTP verbs, resource paths, query parameters, and bodies. |

These follow the [Connect introduction](https://connectrpc.com/docs/introduction/), [Connect protocol](https://connectrpc.com/docs/protocol/), [Connect-Web guide](https://connectrpc.com/docs/web/getting-started/), and [gateway documentation/source](https://github.com/grpc-ecosystem/grpc-gateway). HTTP JSON and REST resource mapping are separate requirements.

## Streaming and large messages

Connect-Go supports client, server, and bidirectional streaming, including gRPC interoperability. Bidi requires end-to-end HTTP/2. Connect-Web currently supports unary and server streaming, not browser client streaming or bidi. Gateway translates supported streaming APIs into newline-delimited JSON, but does not provide true bidi. Thus FRY's member transport can use native protobuf streaming with either Go framework; browser support does not decide the internal transport. [Go streaming](https://connectrpc.com/docs/go/streaming/), [browser limitation](https://connectrpc.com/docs/web/getting-started/), [gateway features](https://github.com/grpc-ecosystem/grpc-gateway#features).

For approximately 200 MB email payloads, proposed design: bounded protobuf `bytes` chunks on internal streams, with explicit per-message and aggregate limits, cancellation, integrity checks, and application acknowledgement semantics. An HTTP streaming body does not subdivide a single protobuf message automatically. grpc-go defaults to a 4 MB receive-message limit on server and client; Connect's documented v1 `WithReadMaxBytes` limit is per message and defaults to unlimited. Configure deliberate limits for the selected major version. ProtoJSON encodes bytes as base64, adding roughly one-third payload size, so prefer binary protobuf internally. Chunking bounds individual messages; bounded memory also requires avoiding application accumulation. These are design inferences, not measured throughput claims. [grpc-go limits](https://pkg.go.dev/google.golang.org/grpc#MaxRecvMsgSize), [client limit](https://pkg.go.dev/google.golang.org/grpc#MaxCallRecvMsgSize), [Connect v1 limits](https://pkg.go.dev/connectrpc.com/connect#WithReadMaxBytes), [ProtoJSON](https://protobuf.dev/programming-guides/json/).

## Authentication, validation, and telemetry

Connect supports interceptors around unary and streaming calls; its validation package integrates Protovalidate. grpc-go exposes unary and stream server interceptors, allowing explicit authentication and validation integration. Neither framework supplies FRY's authorization policy merely by generating protobuf code. Proposed policy: authenticate member identity at the transport boundary, authorize each operation, and validate every incoming stream message. [Connect interceptors](https://connectrpc.com/docs/go/interceptors/), [Connect validation](https://github.com/connectrpc/validate-go), [grpc-go server options](https://pkg.go.dev/google.golang.org/grpc#UnaryInterceptor), [stream interceptors](https://pkg.go.dev/google.golang.org/grpc#StreamInterceptor).

Gateway does **not** require a separate process or network hop: generated `Register…HandlerServer` functions call the service implementation directly. Their documented caveats are material: gRPC interceptors do not run and streaming calls are unsupported by that in-process registration. HTTP middleware/shared authorization must cover that path. Using the gateway's normal gRPC client registration preserves the gRPC server execution path and its interceptors, at the cost of a transport hop. [Generated registration and streaming rejection](https://github.com/grpc-ecosystem/grpc-gateway/blob/main/examples/internal/proto/examplepb/stream.pb.gw.go).

Connect's current documentation covers v2: registration uses `connect.Server` plus `connecthttp.Mount`, and interceptors wrap unary and streaming through unified client/server function types. V1's generated HTTP-handler constructors and three-method interceptor interface differ. Choose a major version and verify matching validation/telemetry APIs before implementation; the v1 size-limit reference above must not be copied as a v2 configuration recipe. [Official v2 migration guide](https://connectrpc.com/docs/go/migrating-to-v2/).

Both stacks integrate with OpenTelemetry: Connect documents RPC instrumentation through `otelconnect` and HTTP instrumentation through `otelhttp`; grpc-go uses `otelgrpc` stats handlers. Supply FRY's providers/propagators explicitly and review emitted attributes. Instrumentation availability is not a differentiator. [Connect observability](https://connectrpc.com/docs/go/observability/), [otelgrpc](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc).

## OpenAPI and conditional recommendation

Gateway provides protobuf-to-OpenAPI tooling for its mapped HTTP interface. OpenAPI is not impossible with Connect: Connect explicitly supports interoperability with grpc-gateway, so a resource HTTP/OpenAPI facade can target a Connect server over gRPC. That adds the gateway mapping/generation layer and should be an explicit requirement, rather than an assumed property of native Connect RPC routes. [Gateway generators](https://github.com/grpc-ecosystem/grpc-gateway), [Connect interoperability](https://connectrpc.com/).

Given the confirmed HTTP requirement, recommend Connect-Go for administrative and member services, Connect-Web for the browser, and native gRPC protocol for member streaming. The user accepted this framework choice after reviewing the comparison. If resource-oriented HTTP routes and their generated OpenAPI contract become required, reconsider grpc-go + grpc-gateway; Connect + gateway also remains viable. Keep administrative and member services separately registered and authenticated regardless of framework. High-volume streaming performance remains unverified for both options.
