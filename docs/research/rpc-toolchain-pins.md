# RPC toolchain pins

Investigated **2026-10-18**, using official release APIs, publisher package metadata,
tagged source and official documentation. Read alongside
[transport research](api-transport-options.md), [API contract](../design/api-contract.md)
and [implemented telemetry policy](../design/sql-and-telemetry.md).

This is a historical research record. The repository now has Git history,
implemented Connect/gRPC transports and native generation. See the
[RPC tooling reference](../../tooling/rpc/README.md) and
[CI recovery record](../validation/ci-recovery.md) for the later workflow and
validation; the snapshot findings below are not its current implementation status.

During this research, no dependencies were installed, generators executed,
application/tooling files changed, or artifacts published. Compatibility is
supported by dependency declarations and inspected APIs; it is **not a compiled
or end-to-end tested compatibility claim**. The inspected source snapshot had no `.git`
metadata, so no FRY commit ID can be asserted. The existing coverage manifest
`.artifacts/foundation/source.sha256` has SHA-256
`e7ebfef4e58b822bf861893c2a4b71a86a4aa5f1a6a61523d25fe457f0dfd0a6`;
its entries passed `sha256sum --check --quiet` before this note was written.
A final read-only recheck reported six mismatches: `internal/admin/mail.go`,
`internal/smtpserver/io_test.go`, `internal/smtpserver/transport_test.go`,
`third_party/go-imap/FRY-PATCHES.md`,
`third_party/go-imap/imapserver/imapmemserver/message.go`, and
`third_party/go-imap/imapserver/message.go`. This research wrote only this note;
those source changes were outside this work. The manifest was not refreshed, and
this note makes no new coverage claim for the changed snapshot.

## Recommended set

Use **Connect-Go v2**, local Buf compilation and local pinned plugins in a
Docker/Make workflow. Generate both Go and TypeScript into Git, bundle the vanilla
browser UI into its existing embedded `app.js`, and select binary gRPC explicitly
for internal mTLS calls. Keep protobuf and OTel at the existing pins. Do not add
`protoc-gen-connect-es`: Connect-Web v2 consumes protobuf-es service descriptors.

The GitHub `/releases/latest` endpoints returned the releases below with
`prerelease: false` and publication dates no later than the research date. npm
`latest` independently agreed for the listed JavaScript packages. These are
latest non-prerelease releases; **otelconnect and esbuild remain pre-1.0 APIs**.

| Component | Exact pin | Release evidence and date |
| --- | --- | --- |
| Connect-Go runtime and generator | `connectrpc.com/connect/v2 v2.0.1`; `connectrpc.com/connect/v2/cmd/protoc-gen-connect-go@v2.0.1` | [Release](https://github.com/connectrpc/connect-go/releases/tag/v2.0.1), [API](https://api.github.com/repos/connectrpc/connect-go/releases/latest), Oct 14 |
| Go protobuf runtime and generator | `google.golang.org/protobuf v1.36.12`; `google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12` | [Release](https://github.com/protocolbuffers/protobuf-go/releases/tag/v1.36.12), [API](https://api.github.com/repos/protocolbuffers/protobuf-go/releases/latest), Aug 10; runtime already pinned |
| Buf CLI/compiler | `v1.74.0` | [Release](https://github.com/bufbuild/buf/releases/tag/v1.74.0), [API](https://api.github.com/repos/bufbuild/buf/releases/latest), Oct 14 |
| Standalone protoc, alternative compiler | `36.2` (Git tag `v36.2`) | [Release](https://github.com/protocolbuffers/protobuf/releases/tag/v36.2), [API](https://api.github.com/repos/protocolbuffers/protobuf/releases/latest), Sep 17; unnecessary for the recommended Buf path |
| Browser RPC runtimes | `@connectrpc/connect@2.2.0`, `@connectrpc/connect-web@2.2.0` | [Release](https://github.com/connectrpc/connect-es/releases/tag/v2.2.0), [API](https://api.github.com/repos/connectrpc/connect-es/releases/latest), Sep 7 |
| TS generator and protobuf runtime | `@bufbuild/protoc-gen-es@2.16.0`, `@bufbuild/protobuf@2.16.0` | [Release](https://github.com/bufbuild/protobuf-es/releases/tag/v2.16.0), [API](https://api.github.com/repos/bufbuild/protobuf-es/releases/latest), Sep 29 |
| Browser bundler | `esbuild@0.28.2` | [Release](https://github.com/evanw/esbuild/releases/tag/v0.28.2), [API](https://api.github.com/repos/evanw/esbuild/releases/latest), Aug 8 |
| Type checker | `typescript@7.0.2` | [Release](https://github.com/microsoft/TypeScript/releases/tag/v7.0.2), [API](https://api.github.com/repos/microsoft/TypeScript/releases/latest), Aug 20 |
| Connect OTel integration, optional with restrictions below | `connectrpc.com/otelconnect v0.12.0` | [Release](https://github.com/connectrpc/otelconnect-go/releases/tag/v0.12.0), [API](https://api.github.com/repos/connectrpc/otelconnect-go/releases/latest), Oct 8 |
| OTel API, trace, metric, SDK, SDK metric and existing HTTP exporters | Retain `v1.47.0` | [Existing research](otel-sqlc-foundation.md), [official release](https://github.com/open-telemetry/opentelemetry-go/releases/tag/v1.47.0); no upgrade needed for this integration |

Retain existing Go **1.27.2** and Node **26.12.0** build images from
`tooling/versions.env`. Node's [official distribution index](https://nodejs.org/dist/index.json)
identifies 26.12.0 as Current, not LTS; the latest LTS is 24.22.0. There is no need
to change FRY's established Node baseline: the generator requires Node >=22.
Node is a build/test dependency, not an extra production service.

### Why these versions fit

- Connect-Go 2.0.1 requires Go 1.26.0 and protobuf 1.36.11. FRY's Go 1.27.2 and
  protobuf 1.36.12 exceed these requirements. Buf 1.74.0 requires Go 1.26.7 when
  built from source. [Connect module](https://github.com/connectrpc/connect-go/blob/v2.0.1/go.mod),
  [Buf module](https://github.com/bufbuild/buf/blob/v1.74.0/go.mod).
- otelconnect 0.12.0 requires Connect v2.0.0, protobuf 1.36.11 and OTel 1.46.0.
  Go module selection can retain Connect 2.0.1, protobuf 1.36.12 and OTel 1.47.0.
  It also imports Connect **v1.19.1** for its compatibility API: seeing both major
  modules in the graph would be expected, not a reason to generate v1 handlers.
  [Tagged module](https://github.com/connectrpc/otelconnect-go/blob/v0.12.0/go.mod).
- Connect-Web 2.2.0 requires exactly Connect 2.2.0 and protobuf `^2.7.0`;
  Connect itself accepts protobuf `^2.7.0`. Protobuf 2.16.0 satisfies both.
  [Web manifest](https://github.com/connectrpc/connect-es/blob/v2.2.0/packages/connect-web/package.json),
  [Connect manifest](https://github.com/connectrpc/connect-es/blob/v2.2.0/packages/connect/package.json).
- The ES generator depends on protobuf and protoplugin **exactly 2.16.0** and
  requires Node >=22. Pin the generator and runtime together. Its npm manifest
  confirms the published requirements. [Tagged generator manifest](https://github.com/bufbuild/protobuf-es/blob/v2.16.0/packages/protoc-gen-es/package.json),
  [publisher metadata](https://registry.npmjs.org/@bufbuild/protoc-gen-es/2.16.0).
- Connect v2 removes the separate TS Connect generator and uses plain protobuf
  message shapes and service descriptors. Use `createClient`, not the v1
  `createPromiseClient`/generated message-class patterns.
  [Tagged migration guide](https://github.com/connectrpc/connect-es/blob/v2.2.0/MIGRATING.md).

Start FRY definitions with `syntax = "proto3"`; do not infer support for a newer
protobuf Edition merely from protoc's release number. Buf's compiler and protoc
are alternatives, and protoc's version is not the Go runtime's version.

## Local generation versus remote plugins

| Choice | Reproducibility and operation | FRY recommendation |
| --- | --- | --- |
| Buf 1.74.0 + local executables | Buf compiles schemas; plugins run locally. Pin binary checksums, Go plugin module versions, npm lockfile, options, inputs and image digest. With dependencies staged, generation can run with network disabled and no BSR account/token. | Preferred: local lint/breaking checks and generation through Make/Docker. |
| protoc 36.2 + the same local plugins | Also local and token-free. Must explicitly maintain include paths, sorted input list and lint/breaking tooling. Pin compiler/WKT archive too. | Valid alternative; avoid using both compilers as interchangeable canonical output. |
| Buf remote plugins | Configure plugin version **and revision**; hosted execution takes generator installation off the worker, but requires network and sends descriptors to the remote service. | Optional only, not a mandatory build path for this private/self-hosted project. |

Buf distinguishes `local`, `remote`, and `protoc_builtin`; `protoc-gen-go` is an
external executable, not a protoc built-in. A local plugin can be an executable
path or command list. Buf normally has its own compiler, so the recommended
configuration does not require installing protoc. Remote version plus revision
pins the hosted packaging, not a locally inspected binary digest.
[Generation reference](https://buf.build/docs/configuration/v2/buf-gen-yaml/),
[compiler documentation](https://buf.build/docs/reference/internal-compiler/).

Do not claim that all remote generation requires a token: public plugin access
and private registry authorization are distinct. Current access limits and
credentials would need checking for the chosen BSR deployment. Official remote
examples show `buf.build/protocolbuffers/go`, `buf.build/connectrpc/go` and
`buf.build/bufbuild/es`. This research has **not verified that the precise versions
above and a particular revision are published there**, so it supplies no invented
remote revision pin. [Remote usage](https://buf.build/docs/bsr/remote-plugins/usage/).

For a future implementation, build the generator image once from pinned inputs,
retain it in the installation's chosen registry/archive, and pin its final digest.
Use ordinary Git and Make; no hosted CI product is required. Populate Go module
checksums and npm's complete lockfile during the implementation step, retain
licenses, then run generation without network. Tool bootstrap may require public
module/package downloads or a local mirror; token-free generation does not mean
an empty machine can bootstrap offline.

### Configuration skeletons — illustrative, not added to the repository

Use dedicated generated directories so cleaning never deletes handwritten code.
Assume `proto/fry/admin/v1/admin.proto` declares
`option go_package = "fry/internal/gen/fry/admin/v1;adminv1";`.

```yaml
# buf.yaml
version: v2
modules:
  - path: proto
lint:
  use: [STANDARD]
breaking:
  use: [FILE]
```

```yaml
# buf.gen.yaml; executable locations are fixed inside the generator image.
version: v2
plugins:
  - local: /opt/fry-tools/protoc-gen-go
    out: internal/gen
    opt: [paths=source_relative]
  - local: /opt/fry-tools/protoc-gen-connect-go
    out: internal/gen
    opt: [paths=source_relative]
  - local: /opt/fry-ui/node_modules/.bin/protoc-gen-es
    out: ui/gen
    opt: [target=ts, import_extension=js]
inputs:
  - directory: proto
```

`paths=source_relative` keeps package paths under these output roots. The Go
Connect plugin v2 generates `Register…Handler(server, implementation)` and a
client constructor taking `*connect.Client`; it does not generate the v1 HTTP
handler constructor. [Tagged generator](https://github.com/connectrpc/connect-go/blob/v2.0.1/cmd/protoc-gen-connect-go/main.go),
[Go generation paths](https://protobuf.dev/reference/go/go-generated/).

The corresponding tool-bootstrap commands, **not executed here**, would use
`GOBIN=/opt/fry-tools go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12`
and
`GOBIN=/opt/fry-tools go install connectrpc.com/connect/v2/cmd/protoc-gen-connect-go@v2.0.1`.
Run these in an isolated tools build stage, not through `go get` in FRY's runtime
module. Pin the tools stage's dependency graph/checksums as well; a versioned
`go install` command alone is not a full artifact manifest.

For a protoc alternative, use the same binaries/options, specify
`--plugin=protoc-gen-go=/opt/fry-tools/protoc-gen-go` and equivalent Connect/ES
paths, `-I proto`, `--go_out=internal/gen`,
`--go_opt=paths=source_relative`, `--connect-go_out=internal/gen`,
`--connect-go_opt=paths=source_relative`, `--es_out=ui/gen`,
`--es_opt=target=ts,import_extension=js`, and an explicit sorted list of FRY proto
files. Include protoc's pinned `include/` directory if importing well-known types.

## Vanilla embedded UI

The current server embeds `internal/admin/web/*`; HTML loads `/app.js` with
`defer`, and the HTTP adapter serves that fixed asset path. Keep this delivery
shape: generate TS into a source directory, then bundle a handwritten TS entry
and generated descriptors into **one browser IIFE** at the embedded `app.js`
path. No React, dev server, CDN imports or production Node runtime is needed.
Existing UI tests read `web/app.js`; moving its source means adapting those tests
when implementing the build, not assuming they continue to inspect handwritten JS.
[Local adapter](../../internal/admintransport/http.go), [HTML](../../internal/admintransport/web/index.html).

```json
{
  "private": true,
  "dependencies": {
    "@bufbuild/protobuf": "2.16.0",
    "@connectrpc/connect": "2.2.0",
    "@connectrpc/connect-web": "2.2.0"
  },
  "devDependencies": {
    "@bufbuild/protoc-gen-es": "2.16.0",
    "esbuild": "0.28.2",
    "typescript": "7.0.2"
  },
  "scripts": {
    "typecheck": "tsc --noEmit",
    "build": "esbuild app.ts --bundle --platform=browser --format=iife --target=es2022 --minify --outfile=../internal/admin/web/app.js"
  }
}
```

This example assumes a package rooted at `ui/`, containing `app.ts`, `gen/`, and:

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "module": "ESNext",
    "moduleResolution": "Bundler",
    "lib": ["ES2022", "DOM", "DOM.Iterable"],
    "strict": true,
    "isolatedModules": true,
    "verbatimModuleSyntax": true,
    "noEmit": true
  },
  "include": ["**/*.ts"]
}
```

Use `npm ci` against a committed lockfile before generation/typecheck/build.
Do not use floating versions or a generator downloaded implicitly by `npx`.
TypeScript 7.0.2 and esbuild 0.28.2 are independently verified releases, but their
operation on FRY's eventual TS source remains a validation gate. esbuild removes
types without checking them; run the separate checker. `ES2022` is a proposed
browser baseline, not a verified FRY browser-support promise.
[esbuild TypeScript behavior](https://esbuild.github.io/content-types/#typescript),
[TypeScript options](https://www.typescriptlang.org/tsconfig/).

```ts
import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { AdminService } from "./gen/fry/admin/v1/admin_pb.js";

const transport = createConnectTransport({
  baseUrl: window.location.origin,
  useBinaryFormat: true,
  fetch: (input, init) => fetch(input, { ...init, credentials: "same-origin" }),
});
const admin = createClient(AdminService, transport);
// admin.<generatedUnaryMethod>({...}, { signal, timeoutMs });
```

`AdminService` is an illustrative schema name. Connect-Web's binary format is an
explicit choice; 2.2.0 defaults to JSON. It takes a custom `fetch` for credentials,
not a transport-level `credentials` option. Preserve existing session/CSRF checks
and map their headers deliberately. Binary browser calls do not remove the
server's HTTP/JSON support. Use the accepted authenticated HTTP download path for
original mail bytes. [Tagged browser transport](https://github.com/connectrpc/connect-es/blob/v2.2.0/packages/connect-web/src/connect-transport.ts),
[client API](https://connectrpc.com/docs/web/using-clients/).

## Go v2 transport and internal streaming

These fragments assume generated services and existing authenticated HTTP setup;
they show version-correct registration/client construction, not a complete server.

```go
import (
    "net/http"
    "connectrpc.com/connect/v2"
    "connectrpc.com/connect/v2/connecthttp"
)

adminServer := connect.NewServer(/* authentication/application interceptors */)
adminv1connect.RegisterAdminServiceHandler(adminServer, adminImplementation)
adminMux := http.NewServeMux()
connecthttp.Mount(adminMux, adminServer,
    connecthttp.WithReadMaxBytes(adminMessageLimit),
    connecthttp.WithSendMaxBytes(adminMessageLimit),
)

memberServer := connect.NewServer(/* internal identity/authorization interceptors */)
memberv1connect.RegisterMemberServiceHandler(memberServer, memberImplementation)
memberMux := http.NewServeMux()
connecthttp.Mount(memberMux, memberServer,
    connecthttp.WithReadMaxBytes(memberMessageLimit),
    connecthttp.WithSendMaxBytes(memberMessageLimit),
)

transport := connecthttp.NewTransport(mtlsHTTPClient, memberBaseURL,
    connecthttp.WithGRPC(),
    connecthttp.WithReadMaxBytes(memberMessageLimit),
    connecthttp.WithSendMaxBytes(memberMessageLimit),
)
member := memberv1connect.NewMemberServiceClient(connect.NewClient(transport))
```

The tagged [HTTP transport](https://github.com/connectrpc/connect-go/blob/v2.0.1/connecthttp/connecthttp.go)
and [core constructors](https://github.com/connectrpc/connect-go/blob/v2.0.1/connect.go)
confirm these APIs. A generated Connect client speaking standard gRPC does not
need grpc-go or `protoc-gen-go-grpc`; those are only needed if FRY separately
chooses grpc-go clients/servers. Existing indirect grpc-go in `go.mod` is not that
decision.

`mtlsHTTPClient` must use an HTTP/2-enabled TLS transport, client certificate and
trusted peer roots/server-name verification. Configure the **separate member
listener** with `tls.RequireAndVerifyClientCert`, trusted client CAs and h2, then
bind verified certificate identity to member authorization. Never use
`InsecureSkipVerify`. Transport authentication does not authorize every method.
[Go TLS configuration](https://pkg.go.dev/crypto/tls#Config),
[Connect deployment](https://connectrpc.com/docs/go/deployment/).

In v2 the size options belong to **connecthttp**, not core connect. The read
limit now defaults to **4 MiB per message**; the send limit defaults to unlimited.
Specify both at both ends. Set a chunk payload limit lower than the encoded
message limit to leave room for metadata. No fixed chunk number is established by
this research. [Tagged size options](https://github.com/connectrpc/connect-go/blob/v2.0.1/connecthttp/option.go).

Keep aggregate byte count, chunk ordering/offset, final length/digest, concurrency,
deadline, cancellation and incomplete-transfer cleanup in the transfer contract.
Bounded messages do not bound total memory if code appends them all. Stream through
bounded buffers, reject before accepting excessive aggregate bytes, and require
an explicit durable completion acknowledgement. EOF or transport success alone
cannot grant durable acceptance. Browser unary metadata and HTTP original
downloads remain separate from this internal stream.
[Accepted contract](../design/api-contract.md), [streaming](https://connectrpc.com/docs/go/streaming/).

## OTel compatibility and privacy gate

The correct v2 constructors are `otelconnect.NewServerInterceptor` and
`otelconnect.NewClientInterceptor`; `NewInterceptor` is the retained **v1** API.
Provider/propagation options are `WithTracerProvider`, `WithMeterProvider` and
**`WithPropagator` (singular)**. Pass process-owned providers explicitly; omitted
options read globals. [Release/API example](https://github.com/connectrpc/otelconnect-go/releases/tag/v0.12.0),
[tagged options](https://github.com/connectrpc/otelconnect-go/blob/v0.12.0/option.go).

There is a concrete privacy blocker to blindly enabling tracing: otelconnect
copies Connect error messages (or `err.Error()`) into span status descriptions.
`WithAttributeFilter` does not sanitize those descriptions. Untrusted remote
parents can also become span links; merely declining `WithTrustRemote` does not
mean incoming context is discarded.
[Tagged status and extraction implementation](https://github.com/connectrpc/otelconnect-go/blob/v0.12.0/interceptor.go).

Default request attributes are bounded RPC system/method; outcomes use canonical
status and error type. Client peer address attributes, optional headers and
application-added `Labeler` attributes require separate care. Header and Labeler
paths cannot be assumed covered by the ordinary attribute filter.
[Attribute construction](https://github.com/connectrpc/otelconnect-go/blob/v0.12.0/attributes.go),
[recording implementation](https://github.com/connectrpc/otelconnect-go/blob/v0.12.0/interceptor.go).

**Recommended first implementation:** keep FRY's existing narrow telemetry owner
and extend it with a v2 interceptor for fixed RPC procedure/outcome observations;
leave otelconnect uninstalled unless its extra metrics are specifically wanted.
This preserves existing metric names and avoids creating a second SDK/exporter
owner. Full otelconnect tracing is conditional on a tested privacy adapter that
removes status descriptions, unsafe attributes/events/links and opaque tracestate
before export, without altering application errors. Provider compatibility alone
does not establish privacy compatibility.

If standard RPC metrics are wanted first, this version-correct construction
explicitly disables otelconnect tracing while retaining an owned meter provider:

```go
import (
    "connectrpc.com/connect/v2"
    "connectrpc.com/otelconnect"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/propagation"
    tracenoop "go.opentelemetry.io/otel/trace/noop"
)

options := []otelconnect.Option{
    otelconnect.WithTracerProvider(tracenoop.NewTracerProvider()),
    otelconnect.WithMeterProvider(processOwnedMeterProvider),
    otelconnect.WithPropagator(propagation.NewCompositeTextMapPropagator()),
    otelconnect.WithAttributeFilter(func(_ connect.Spec, kv attribute.KeyValue) bool {
        switch string(kv.Key) {
        case "rpc.system.name", "rpc.method", "rpc.response.status_code", "error.type":
            return true
        default:
            return false
        }
    }),
}
serverMetrics, err := otelconnect.NewServerInterceptor(options...)
if err != nil { return err }
server := connect.NewServer(serverMetrics)
// Register generated services and mount using connecthttp as above.
```

This fragment deliberately performs no propagation or RPC tracing. Do not add
header collection or Labeler values. In disabled mode supply the metric no-op
provider too. A full tracing variant would replace the no-op tracer with FRY's
privacy-enforcing provider adapter and configure propagation by trust boundary;
that adapter does not exist in today's `telemetry.Runtime`, which exposes
operations rather than provider accessors.

For public administration, discard incoming trace context entirely. For
**authenticated internal** calls, preserve W3C trace IDs/flags while stripping
tracestate; accept no baggage. Do not use a bare `propagation.TraceContext{}` as a
claim of tracestate sanitization. Reuse process resources/exporters, disabled
behavior and bounded shutdown, and avoid a generic otelhttp wrapper that adds URL
attributes. [FRY runtime](../../internal/telemetry/telemetry.go),
[FRY instrumentation policy](../design/sql-and-telemetry.md).

## Artifact identity evidence

The following commits were resolved through GitHub's official
`/repos/{owner}/{repo}/commits/{tag}` API on the research date. They identify source
content; they are not binary archive checksums or image digests.

| Release | Resolved source commit |
| --- | --- |
| [Connect-Go 2.0.1](https://api.github.com/repos/connectrpc/connect-go/commits/v2.0.1) | `f86de812094e37f7e975f969c8cc85818cbbc9d7` |
| [Connect-ES 2.2.0](https://api.github.com/repos/connectrpc/connect-es/commits/v2.2.0) | `378bb8fde1209afc3ab6a9b0aa97c00d0db58c24` |
| [protobuf-es 2.16.0](https://api.github.com/repos/bufbuild/protobuf-es/commits/v2.16.0) | `b3b232f7f466eda27c66e8f63c82f930578b6cc6` |
| [otelconnect 0.12.0](https://api.github.com/repos/connectrpc/otelconnect-go/commits/v0.12.0) | `20d381df2ddc91d0960563afad532ae9653d9659` |
| [Buf 1.74.0](https://api.github.com/repos/bufbuild/buf/commits/v1.74.0) | `36fdbc523bcbda32e63aaa31aaa147d40e480307` |
| [protoc 36.2](https://api.github.com/repos/protocolbuffers/protobuf/commits/v36.2) | `2c74169b34066ceb8ddb6b882fcb3fb32d737a55` |
| [protobuf-go 1.36.12](https://api.github.com/repos/protocolbuffers/protobuf-go/commits/v1.36.12) | `cdd4c5f7406e82462949c7a65defa9f3029c162d` |
| [TypeScript 7.0.2](https://api.github.com/repos/microsoft/TypeScript/commits/v7.0.2) | `1e4744d68260a7cb91b62b12edc3f6a2187faaf1` |
| [esbuild 0.28.2](https://github.com/evanw/esbuild/releases/tag/v0.28.2) | Release target commit `609683d892977362a0f99026cb74b96263d728a9` |

Official release API asset `digest` fields supplied these **archive/binary SHA-256**
values. The archives were not downloaded/rehashed in this research. Verify them
when bootstrapping; choose the correct architecture rather than reusing a digest
across platforms.

| Official release asset | SHA-256 |
| --- | --- |
| [buf-Linux-x86_64](https://github.com/bufbuild/buf/releases/download/v1.74.0/buf-Linux-x86_64) | `92326d957d613712669fb55f9e4340946a8142a92120e09008459118778273d2` |
| [buf-Linux-aarch64](https://github.com/bufbuild/buf/releases/download/v1.74.0/buf-Linux-aarch64) | `d42b471501a85dbadb40062960492dd81ce5ca4e413f4133d514d3ae3c0af4df` |
| [Connect generator Linux-x86_64 archive](https://github.com/connectrpc/connect-go/releases/download/v2.0.1/protoc-gen-connect-go-v2.0.1-Linux-x86_64.tar.gz) | `35bcf3feb82d29aaef8a40aae87e6a5738fbfc684e727e13e481428726c6b6b7` |
| [Connect generator Linux-aarch64 archive](https://github.com/connectrpc/connect-go/releases/download/v2.0.1/protoc-gen-connect-go-v2.0.1-Linux-aarch64.tar.gz) | `d4807adcd979bc6b41cc929de3de4b5a808001805937bd94c3713251b0a40cf7` |
| [protoc Linux-x86_64 archive](https://github.com/protocolbuffers/protobuf/releases/download/v36.2/protoc-36.2-linux-x86_64.zip) | `121f6c7afe1d4d0e3ea6aab9432038599250134cbf4474cb1167d2c7decd4278` |
| [protoc Linux-aarch_64 archive](https://github.com/protocolbuffers/protobuf/releases/download/v36.2/protoc-36.2-linux-aarch_64.zip) | `8b8f18bd2b30346efbc698dd5a73dd7c805f3ef8380f6dfc95c768f3f1852f6a` |

Publisher npm metadata supplied these package SRI hashes; a future lockfile must
also contain **all transitive and platform-specific packages**, including esbuild
and TypeScript native binaries. These six entries alone are not a lockfile.

| Package metadata | `dist.integrity` |
| --- | --- |
| [connect 2.2.0](https://registry.npmjs.org/@connectrpc/connect/2.2.0) | `sha512-M63OQ1ZFcPUm85CuewJ9G9CMV8LjzGR9935xOnXNQGSazAbwMb3x2W7vXBYM1zqrP7XFD8uxp/dUAhirclgwnQ==` |
| [connect-web 2.2.0](https://registry.npmjs.org/@connectrpc/connect-web/2.2.0) | `sha512-uuBUcBTYfb/r1o98LzCCZk1H90yx0vmZ5m21dpw/q4EUwsKwkCDGI8H0aEdjcZld+Q2jJ5A5ADvPqfb6lzQ/sg==` |
| [protobuf 2.16.0](https://registry.npmjs.org/@bufbuild/protobuf/2.16.0) | `sha512-FWa0sPlqYGJgpTs6OxBRcL/AW4JT0OIO+W1ajerluoTVh8CV0B7XM1qsNCRgnkSCkRCgOICKHRQlY17jrsm0+Q==` |
| [protoc-gen-es 2.16.0](https://registry.npmjs.org/@bufbuild/protoc-gen-es/2.16.0) | `sha512-LbPrEaGd7bOxKbJHllTR5439+PZjqd6fM7xyDZkDVtZ+Efzr7TMdMqShG9Qkf/nYVyiNS/SK5r9JhRaBTFkfZw==` |
| [esbuild 0.28.2](https://registry.npmjs.org/esbuild/0.28.2) | `sha512-HKVLS8dvII+xoKW9kmqxbRKrnWEXfJJr/FZhhJmiqIB0e053QNYFqOBouTMO/k5sID4MvCiUCvv8b9M4h32wIA==` |
| [typescript 7.0.2](https://registry.npmjs.org/typescript/7.0.2) | `sha512-8FYau96o3NKOhbjKi/qNvG/W5jhzxkbdm5sj9AbZ/5T5sWqn3hJgLfGx27sRKZWTvyzCP8dLRBTf5tBTSRVUNA==` |

FRY's existing `go.sum` already records protobuf 1.36.12 module checksum
`h1:pJOKDDOyeXErUroCihFAd5LQuwXBSpVnKGrj5o/fwxc=`. Do not substitute an archive
SHA-256 for Go's module checksum. New Connect/otelconnect module checksums and a
combined generator-image digest remain to be captured during authorized
implementation; no image was built here and no digest is invented. The existing
Node image pin is
`node:26.12.0-bookworm@sha256:a856ae5e25d498bbe5ec85fde88f00f5ed99908fda803ba39d7735ab8574f58c`
from `tooling/versions.env`; this note does not independently re-resolve it.

## Remaining implementation evidence

The exact dependency declarations establish a coherent candidate set. Before
calling it an implemented foundation, generate a tiny proto3 unary/streaming
fixture with the pinned local tools, typecheck/bundle it, compile with FRY's
existing module pins, and exercise browser Connect plus standard-gRPC mTLS calls.
Check deterministic regeneration in a temporary tree, separately on amd64/arm64
where native tools differ. Retain hashes and compare generated artifacts.

Exercise oversized and compressed messages, aggregate overrun, cancellation,
truncation, checksum mismatch and failure before durable acknowledgement. Test
cross-listener isolation and peer authorization. Inspect real exported OTel
protobuf for raw errors, headers, IDs, URL/peer data, Labeler values, tracestate,
baggage and public-parent links, with hostile environment variables and telemetry
disabled. These are future validation gates, not tests performed for this note.

No Protovalidate integration/version, remote-plugin revision, RPC chunk size or
new API permission/operation contract has been silently selected. If validation
annotations are introduced, verify the matching stable Connect-v2 validation
integration and vendor/pin schema dependencies locally; the current evidence does
not establish those additional pins.

### Follow-up constructor compilation

The coordinating agent subsequently compiled and ran the Go v2 server/mount and
gRPC-client construction skeleton in `.artifacts/rpc-toolchain`, using the pinned
Go 1.27.2 container, Connect-Go 2.0.1 and protobuf 1.36.12. The isolated module has
its own `go.mod`/`go.sum`; FRY's runtime dependencies were untouched. This verifies
those constructor signatures only. It does not establish generated-code,
browser, streaming, mTLS or telemetry interoperability; those gates above remain.
