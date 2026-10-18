# Administration RPC reference

Status: working prototype, 2026-10-18. This reference describes the implemented
administration transport. The additional mechanisms in
[the API contract](../design/api-contract.md) and
[the mechanism design](../design/api-mechanisms.md) are listed explicitly below
when they are not available. This is not the first supported versioned release.

## Contract and generated clients

The protobuf service is `fry.admin.v1.AdminService`. Every method has a distinct,
typed request and response. The source is
[`proto/admin/fry/admin/v1/admin.proto`](../../proto/admin/fry/admin/v1/admin.proto).
No RPC carries an arbitrary JSON document or an original email payload.

Generated artifacts:

- Go messages: `github.com/cotocisternas/fry/gen/admin/go/fry/admin/v1`.
- Go Connect client and handler registration:
  `github.com/cotocisternas/fry/gen/admin/go/fry/admin/v1/adminv1connect`.
- TypeScript messages and service descriptor:
  `gen/admin/ts/fry/admin/v1/admin_pb.ts`. Use the descriptor with Connect-Web's
  `createClient`; it provides the typed client methods.

Generation uses the existing pinned, offline tool image:

```sh
scripts/generate/proto.sh generate proto/admin gen/admin
scripts/generate/proto.sh check proto/admin gen/admin
internal/admintransport/build-web.sh generate
internal/admintransport/build-web.sh check
```

The dedicated `proto/admin` input excludes internal member RPC definitions. Buf
STANDARD lint and descriptor compilation precede generation. The browser bundle
is `internal/admintransport/web/rpc.js`; its hand-written TypeScript source is
`internal/admintransport/browser/rpc.ts`. The build typechecks that source and
bundles the generated descriptor with the pinned Connect-Web runtime. The DOM
screens remain in `web/app.js` and call this bundle. `scripts/generate/proto.sh bootstrap`
installs the pinned native tools and locked generator dependencies through the
repository setup script; generation does not use Docker.

`fry-admin` uses the generated Go client, preserving its existing commands and
flags. Its local command-to-protobuf conversion is not an extra network API.
Each invocation parses its own flags and validates the command before login.
Exit status is zero for success or help, two for invalid command usage, and one
for execution failures. An execution failure can still have an uncertain server
outcome; inspect current state before retrying a mutation.
The unversioned `/api/*` JSON endpoints remain compatibility adapters for existing
prototype automation and regression tests. New CLI/browser metadata requests use
RPC. The binary `/api/content` route remains in use by both.

## Wire protocol and authentication

The configured administration origin serves these methods as Connect HTTP/JSON,
Connect protobuf, gRPC and gRPC-Web. The procedure path is
`/fry.admin.v1.AdminService/<Method>`. Use POST, including for reads. Standard
gRPC clients use HTTP/2; TLS is required off loopback. Listener and reverse-proxy
configuration must preserve HTTP/2 when forwarding gRPC.

All RPC calls require `Origin` equal to the configured administration origin.
The public `GetConfig` and `Login` calls need no cookie. `GetSession` requires a
current cookie but no CSRF header, so a reloaded browser can retrieve its token.
Every other RPC requires the current cookie and `X-FRY-CSRF` from `Login` or
`GetSession`, including reads. A generated client grants no authority: the
application operation rechecks organization, global-operator and content
permissions. Disabling accounts, changing grants or revoking sessions takes
effect on current checks.

`Login` accepts local administrator credentials and sets an HttpOnly,
SameSite=Lax cookie at `/`. HTTPS uses a Secure `__Host-fry-admin` cookie; numeric
HTTP loopback uses `fry-admin-local`. Session tokens are never returned in a
protobuf message. `Logout` revokes the session and expires the cookie. Its
response does not undo a committed action or accepted mail.

OIDC remains the browser redirect flow at `/auth/oidc` and `/auth/callback`. It
retains signed-claim verification, nonce/state/browser binding, PKCE and current
policy reconciliation. Its resulting session uses the same cookie and RPC
permission checks. Issuer discovery and callback handling are not JSON RPCs.
Administrator, SMTP and IMAP credential namespaces remain separate.

Unary requests and responses have 1 MiB limits, with a five-minute request
ceiling and bounded process admission. Unknown JSON and protobuf fields are
rejected, including unknown nested grant fields. Requests never echo passwords;
responses and audits contain no password verifier. RPC metadata and errors must
not be logged with credential values.

Protobuf JSON uses lowerCamelCase by default and accepts protobuf snake_case
field names. `int64` values serialize as decimal strings; storage accounting
fields are explicitly decimal strings too. Preserve that precision in clients.
Timestamp fields are UTC RFC3339 strings with optional fractional seconds.
Optional protobuf scalar fields distinguish absence from zero/false. In
`SetStorageQuota`, an absent or JSON-null `limitBytes` disables the advisory
threshold; a present decimal string sets it. This is an advisory alert policy,
not storage admission or a physical-capacity guarantee.

Lists return `items`. Resource reads/writes return `result`, except `PutUser`
returns `id`, `GetConfig` returns `oidc`, and acknowledgment-only methods return
`ok`. Default protobuf values may be omitted in JSON; generated clients restore
them. `GetMemberStorage` preserves absence for unknown runtime fields instead of
turning them into false/zero observations.

## Implemented methods and effects

“Organization” below means current configuration permission for the named
organization, or a global operator. “Content” is an additional explicit grant;
global configuration permission does not imply content access. Normal database
mutations run their current authorization, business change and audit in the same
application-owned transaction. A success is returned only after commit. The
transport neither opens a second business transaction nor changes receipt
publication/durability rules.

| Method | Permission | Result and committed effect |
| --- | --- | --- |
| `GetConfig` | Public | OIDC availability only; no provider secrets. |
| `Login` | Valid local administrator credentials and current grants | Issues session and login audit atomically. |
| `GetSession` | Current session | Identity, current grants, CSRF token, absolute expiry. |
| `Logout` | Current session plus CSRF | Deletes current session; expires cookie. |
| `ListOrganizations` | Current session | Only organizations visible under current grants. |
| `PutOrganization` | Global to create; organization to rename | Creates/upserts or renames organization. Empty `id` generates an identity. |
| `ListInboxes` | Organization | Inbox IDs and visible message counts. |
| `CreateInbox` | Organization | Creates/ensures the inbox within its organization and appends audit atomically. Empty `id` generates one. |
| `ListDomains` | Organization | Incoming domain configuration. |
| `PutDomain` | Organization | Saves domain ownership, catch-all inbox and enabled state. |
| `ListAliases` | Organization | Incoming address destinations and enabled state. |
| `PutAlias` | Organization | Saves an address and its ordered destination inboxes. |
| `ListMailAccounts` | Organization | IMAP credential metadata, never password/hash. |
| `PutMailAccount` | Organization plus content | Creates/resets IMAP credential, inbox and enabled state; invalidates old credential sessions on their checked operations. |
| `SetMailAccess` | Organization; also content when enabling | Disables/enables IMAP access. |
| `ListSubmissionAccounts` | Organization | Send-only credential identity, explicit sender/domain restrictions and credential generation. |
| `CreateSubmissionAccount` | Organization | Create-only SMTP credential; disabled names remain reserved. Requires caller-supplied password; never returns it. |
| `SetSubmissionPolicy` | Organization | Replaces explicit permitted sender addresses/domains. Does not change password or enabled state. |
| `RotateSubmissionPassword` | Organization | Rotates SMTP verifier without enabling the account. |
| `SetSubmissionAccess` | Organization | Disables/enables send-only SMTP credential. |
| `ListSenderGrants` | Organization | Sender authorization and evidence metadata. |
| `CreateSenderChallenge` | Organization | Replaces the pending DNS challenge for the scope and returns its TXT name/value/expiry. |
| `VerifySender` | Organization | Checks DNS and current challenge/policy before committing grant and audit. No guaranteed background execution. |
| `RevokeSender` | Organization | Revokes future sender acceptance. Already accepted mail stays queued. |
| `OverrideSender` | Global operator, explicit reason | Audited sender authorization without DNS proof. |
| `GetStorageUsage` | Organization | Logical retained-original counters, advisory policy and latest background alert observation. |
| `SetStorageQuota` | Organization | Sets/disables the advisory byte threshold. |
| `GetMemberStorage` | Global operator | Bounded metadata page and separately timed member runtime observation. Rechecks authority before disclosure. |
| `ListMessages` | Organization | Receipt metadata only; no headers/bodies. |
| `ListAudits` | Current session, grant-scoped | Audit metadata visible under current grants. |
| `ListUsers` | Global operator | Administrator identities, local/OIDC provenance, access and grants. No verifiers. |
| `PutUser` | Global operator | Creates/resets local administrator and password; invalidates previous sessions. Preserves an enabled operator. |
| `ReplaceGrants` | Global operator | Replaces local grants; preserves OIDC grants and an enabled operator. |
| `SetEnabled` | Global operator | Changes administrator access and invalidates sessions. Preserves an enabled operator. |

`ListMessages.after` is the last receipt ID, with at most 100 rows per response.
`ListAudits.before` is the last audit ID, with at most 100 rows in descending ID
order. `GetMemberStorage.after` is the last file ID;
`limit` defaults to 64 and accepts 1–64, with `result.nextFile` when another
page is available. Reauthorize each call. These scans do not promise a snapshot,
exact totals or arbitrary page jumps. Other configuration lists retain their
existing application behavior and can hit the response limit; complete bounded
cursor navigation for them remains unavailable.

## Errors, cancellation and uncertain outcomes

| RPC code | Meaning |
| --- | --- |
| `unauthenticated` | Missing, expired or revoked session; sign in again. |
| `permission_denied` | Current scope/content/global permission, Origin or CSRF check failed. |
| `invalid_argument` | Invalid fields, malformed protobuf/JSON, or unknown fields. |
| `already_exists` | Current application conflict, including ownership/credential/sender conflicts. |
| `not_found` | Requested message, credential or member is unavailable in this operation. |
| `resource_exhausted` | Unary message exceeds its configured limit. |
| `canceled`, `deadline_exceeded` | Client disconnected/canceled or deadline elapsed. A mutation may already have committed. |
| `unavailable` | Safe generic service failure. Its underlying database/storage detail is concealed. |

Host/method/process-admission failures can occur before RPC dispatch and return
ordinary HTTP errors. No endpoint exposes another organization's data through a
transport-only authorization shortcut.

There is no automatic mutation retry in the UI or CLI. On a timeout, disconnect,
service error or uncertain commit, inspect current state before deliberately
repeating a mutation. Repeating create requests with empty IDs can create a
second resource. A response-cleanup or logout failure cannot roll back an
acknowledged mutation; the CLI reports cleanup separately. Output-file/stdout
failure after success is reported as a completed command with failed output.

These are synchronous operations. Cancellation can stop in-flight work before
commit; it cannot undo committed effects. `VerifySender` performs external DNS
reads and then an audited database change, not a durable background job.
`GetMemberStorage` observations and binary transfers honor cancellation and
bounds; no transaction is held while a client streams original bytes. There is
no job status/cancel API or automatic resume/recovery claim for these operations.

## Examples

Connect HTTP/JSON against a disposable numeric-loopback installation:

```sh
origin=http://127.0.0.1:8080
# Keep the password and cookie files private; use only synthetic values here.
curl -sS -c cookies.txt -H "Origin: $origin" -H 'Content-Type: application/json' \
  --data '{"username":"operator","password":"disposable-example"}' \
  "$origin/fry.admin.v1.AdminService/Login"
# Read result.csrf from that response; never put the session in a URL.
curl -sS -b cookies.txt -H "Origin: $origin" -H "X-FRY-CSRF: $csrf" \
  -H 'Content-Type: application/json' --data '{}' \
  "$origin/fry.admin.v1.AdminService/ListOrganizations"
curl -sS -b cookies.txt -H "Origin: $origin" -H "X-FRY-CSRF: $csrf" \
  -H 'Content-Type: application/json' \
  --data '{"organization":"20000000000000000000000000000001","limitBytes":"9223372036854775807"}' \
  "$origin/fry.admin.v1.AdminService/SetStorageQuota"
```

Go with a cookie-jar HTTP client after Login:

```go
rpc := adminv1connect.NewAdminServiceClient(connect.NewClient(
    connecthttp.NewTransport(httpClient, origin),
))
ctx, info := connect.NewClientContext(ctx)
info.RequestHeader().Set("Origin", origin)
info.RequestHeader().Set("X-FRY-CSRF", csrf)
response, err := rpc.ListInboxes(ctx, &adminv1.ListInboxesRequest{
    Organization: organization,
})
```

To use gRPC framing with the generated client, pass `connecthttp.WithGRPC()` to
`NewTransport` and use an HTTP/2-capable client. Standard grpc-go clients can
invoke the same fully qualified procedure with the generated request/response
types; attach `origin`, `cookie` and `x-fry-csrf` as outgoing metadata. Install the
server's trusted TLS roots, not insecure certificate-verification overrides.

TypeScript, with a same-origin browser session:

```ts
const rpc = createClient(AdminService, createConnectTransport({baseUrl: location.origin}));
const session = await rpc.getSession({});
const response = await rpc.listInboxes({organization}, {
  headers: {"X-FRY-CSRF": session.result!.csrf},
});
```

Browser fetch supplies Origin. Native clients must send it explicitly. The
built-in browser also refuses redirects and binds cookies to the same origin.

## Original-email HTTP contract

`GET /api/content?organization=<organization-id>&receipt=<receipt-id>` returns
exact original bytes through FRY as `message/rfc822`, with Content-Length and
`Content-Disposition: attachment; filename="message.eml"`. It uses the same
current cookie, separately requires content permission and creates a content
audit intent before opening bytes. The adapter sets no-store, nosniff and
referrer protections. There are no credentials in the URL, redirects to packed
objects, inline protobuf originals or base64 message bodies.

Closing the owned content reader finalizes complete/interrupted auditing. A
write/read failure aborts the HTTP response and is not recorded as complete
client delivery. Bytes written do not prove the client received them. The CLI
creates a new mode-0600 file, refuses overwrite, synchronizes a complete download
and removes its partial output on failure.

This existing full-download route has no HEAD, Range, ETag, If-Match or If-Range
contract yet. It does not support safe append/resume; the CLI never appends a
partial original. Versioned resumable downloads require the storage identity,
range/pin and audit mechanisms described in the accepted design before being
advertised as implemented.

## Unavailable foundations and compatibility

The following accepted design requirements remain follow-up application work:

- Scoped UUIDv7 mutation deduplication, keyed fingerprints, admission cutoff,
  expiry recognition and seven-day completed-result retention. No request ID is
  accepted/returned or durable retry outcome claimed by this transport.
- Revision compare-and-swap for configuration changes. Credential `version`
  fields represent existing generations, not edit-precondition tokens.
- Durable queued jobs, operation progress/reconnection, execution-time authority,
  effect recovery/fencing and safe cancellation checkpoints.
- Scoped administrator service accounts/bearer authentication. Current native
  clients use the local-login cookie flow; SMTP credentials are not admin tokens.
- Authenticated scope/filter-bound cursor tokens with expiry and previous-page
  navigation across all lists. Current raw ID cursors have no signing/expiry.
- Structured stable error reasons/field violations, authorized revision details
  and durable outcome lookup.
- Versioned original-download path and safe resumable ranges/validators/pins.

No compatibility migration is promised for this prototype. After a supported
versioned release, the accepted API-major policy requires behavioral
compatibility within a major and at least six months of supported overlap after
both a stable replacement and a deprecation announcement. Those future
compatibility guarantees must not be inferred from the current `v1` package name.
