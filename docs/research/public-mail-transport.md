# Public mail transport configuration

Investigated 2026-10-17. This is a concrete implementation proposal based on FRY's current adapters and vendored protocol libraries, not a claim that public listeners or deployment controls already exist. No certificates, DNS records, or external accounts are needed to implement and test it locally.

## Proposed listener modes

Use one explicit transport setting per listener, with shared PEM certificate/key configuration where appropriate. Preserve current configuration as `loopback` by default; allow that mode only on a numeric loopback bind address. Do not infer security from the port number. Fail configuration before creating persistent state when a public mode lacks readable, parseable certificate/key files.

| Service | Transport | Conventional port | Policy |
| --- | --- | --- | --- |
| Incoming SMTP | `starttls` | 25 | Advertise STARTTLS; accept local-recipient mail with or without TLS; no SMTP AUTH |
| Submission | `starttls-required` | 587 | Advertise STARTTLS; hide/reject AUTH before TLS; require authentication and existing sender authorization for mail |
| Submission | `tls` | 465 | TLS handshake precedes greeting; authentication and sender authorization required |
| IMAP | `tls` | 993 | TLS handshake precedes greeting; password authentication only within TLS |
| IMAP, optional later | `starttls-required` | 143 | STARTTLS available; LOGIN/AUTHENTICATE rejected before TLS |
| Administration | `tls` | 443 or configured | Direct HTTPS with configured HTTPS origin; existing session/OIDC/CSRF rules |
| Synthetic local services | `loopback` | Configured high ports | Existing plaintext fixtures, restricted to numeric loopback |

Public MX cannot universally require STARTTLS for local delivery. SMTP STARTTLS resets pre-TLS EHLO/authentication/transaction state and clients must issue a fresh EHLO. These rules distinguish incoming MX from authenticated submission. [RFC 3207 §§4–4.3](https://www.rfc-editor.org/rfc/rfc3207.html).

Implicit TLS is preferred for submission/access; 465 and 993 are the standard submission/IMAP implicit ports. Set `MinVersion: tls.VersionTLS12` explicitly and permit newer versions. This proposal does not change outbound MX behavior or implement DANE/MTA-STS. [RFC 8314 §§3–4](https://datatracker.ietf.org/doc/html/rfc8314).

Suggested compatible shape: add `transport`, `tls_cert_file`, `tls_key_file`, and optional bounded `handshake_timeout_seconds` to listener configuration; retain existing `listen`. Incoming listener default remains loopback; submission may accept multiple listener entries for simultaneous 587/465, with one shared authorized account configuration. Prefer independent server instances per submission transport because `AllowInsecureAuth` and TLS options are server-wide. IMAP may similarly use separate instances sharing the store/directory. Do not add forwarded-header or proxy trust modes as part of this work.

## Existing seams and exact authentication gating

[SMTP adapter](../../internal/smtpserver/server.go) has connection tracking, MaxConnections admission, MaxInFlight storage slots, message/recipient limits, read/write idle timeouts, per-ingest context deadlines, and bounded shutdown. It currently requires a loopback listener and sets `AllowInsecureAuth=true` for submission. [Submission](../../internal/smtpserver/submission.go) exposes PLAIN only for configured users and rejects MAIL without authentication or without explicit sender/domain authorization. Keep these restrictions; public incoming mode must have no submission accounts and never become an open relay.

Pinned `go-smtp v0.25.0` already supports `Server.TLSConfig` for STARTTLS and `tls.Listener` for implicit TLS. With `AllowInsecureAuth=false`, EHLO hides AUTH before TLS and `handleAuth` rejects before decoding a SASL initial response or invoking the backend. After TLS, existing PLAIN authentication can be used. Add an explicit TLS-first check at the submission MAIL boundary for a clear 530 response; retain the existing authentication requirement after TLS. The library uses 523/5.7.10 for premature AUTH today; preserve or deliberately normalize this with a small isolated patch if a strict 530 response is required. [Pinned SMTP implementation](../../third_party/go-smtp/conn.go), [SMTP AUTH protocol](https://www.rfc-editor.org/rfc/rfc4954.html).

[IMAP adapter](../../internal/imapserver/server.go) currently sets `InsecureAuth=true`, restricts Serve to loopback, and bounds sessions to 64. Set `InsecureAuth=false` in every public mode. Pinned `go-imap/v2 v2.0.0-beta.8` supports `Options.TLSConfig`, STARTTLS, and an implicit TLS listener; LOGIN and AUTHENTICATE consult TLS state before calling session authentication. Capabilities then include LOGINDISABLED before TLS where applicable and omit STARTTLS once encrypted. The LOGIN decoder still parses supplied arguments before rejecting insecure authentication: a server cannot prevent a broken client from transmitting unsolicited plaintext credentials. The enforceable promise is no credential solicitation, verification, logging, or successful authentication before TLS. Leave protocol debug writers unset. [Pinned LOGIN](../../third_party/go-imap/imapserver/login.go), [AUTHENTICATE](../../third_party/go-imap/imapserver/authenticate.go), [TLS/capabilities](../../third_party/go-imap/imapserver/capability.go), [IMAP TLS/authentication rules](https://datatracker.ietf.org/doc/html/rfc9051).

The [outbound worker](../../internal/outbound/worker.go) already supports verified STARTTLS and implicit TLS, requires relay authentication for these modes, sets TLS 1.2 minimum and ServerName, never falls back to plaintext, and closes transports on context cancellation. Its explicit loopback transport remains suitable for fixtures. Incoming TLS work should preserve these existing semantics.

## TLS wrapping, admission, deadlines, and shutdown

Both protocol libraries inspect the concrete `*tls.Conn` type. **Do not wrap a TLS connection in FRY's current `trackedConn` before passing it to the protocol library.** For SMTP implicit TLS, the correct layering is raw TCP listener → FRY admission/tracking listener → `tls.NewListener` → protocol Serve. Current SMTP Serve always installs its tracking wrapper internally, so simply handing it a TLS listener is incorrect. Track/close the underlying raw sockets and return a visible `*tls.Conn` to the library. On saturated implicit TLS listeners, close silently; never write plaintext SMTP 421 or IMAP BYE into a TLS handshake. Plaintext/STARTTLS SMTP may retain the bounded 421 rejection. [FRY tracking](../../internal/smtpserver/server.go), [SMTP handshake/type check](../../third_party/go-smtp/server.go), [IMAP TLS type check](../../third_party/go-imap/imapserver/conn.go).

Count connections before expensive handshake/authentication work, including incomplete handshakes. Bound total connections and pending handshake work; do not perform a blocking handshake serially in Accept, which would let one slow peer block all admission. A TLS listener itself is lazy and does not establish a timeout. Either use a bounded worker path that performs `HandshakeContext` with a short deadline, or let the protocol goroutine handshake with a deadline already set on the tracked raw transport and clear that deadline on success. Ensure the latter does not get overwritten by the protocol library before handshake. [Go TLS API](https://pkg.go.dev/crypto/tls).

SMTP's implicit handshake currently uses configured read/write timeouts; STARTTLS uses the outstanding connection deadlines, not a dedicated handshake option. With FRY's current one-minute idle setting, handshake work is bounded but can occupy a connection slot for roughly that period. A distinct ten-second STARTTLS handshake budget needs a small vendored timeout hook; setting deadlines only at TCP accept does not cover a later STARTTLS upgrade. Failed STARTTLS must close the session, with no return to plaintext. [Pinned SMTP handshake paths](../../third_party/go-smtp/conn.go), [implicit handshake](../../third_party/go-smtp/server.go).

IMAP command/literal deadlines are currently library constants: command 30 seconds, authenticated idle 35 minutes, literals five minutes. STARTTLS wraps the connection lazily, so actual handshake occurs with subsequent protocol I/O. Make 993 the first public IMAP scope; 143 is technically supported but a distinct upgrade-handshake timeout and failure tests warrant separate work. Keep admission slots active until connections close; pending handshakes must be included. [Pinned IMAP deadlines](../../third_party/go-imap/imapserver/conn.go), [STARTTLS path](../../third_party/go-imap/imapserver/starttls.go).

Load keys with `tls.LoadX509KeyPair`; never log PEM contents, private keys, or credentials. Use normal server certificates with the expected DNS SANs, TLS 1.2 minimum, and no client-certificate requirement for ordinary password users. Loading once at startup is a sufficient initial scope; renewal can restart the process. Certificate acquisition/rotation automation is deployment work. Local tests generate a private CA and leaf certificate and explicitly trust only that CA. [Go TLS certificate API](https://pkg.go.dev/crypto/tls).

Use current cancellation and bounded drain behavior: stop listeners, cancel service contexts, close tracked raw sockets after the drain deadline, then wait for protocol/storage goroutines before closing storage. Handshakes stalled before a protocol session exists must also close during shutdown. Surface asynchronous listener failure through the existing protocol error channel. [SMTP shutdown](../../internal/smtpserver/server.go), [CLI lifecycle](../../cmd/fry/main.go).

## Administration HTTPS

[Admin HTTP handler](../../internal/admintransport/http.go) already accepts an HTTPS origin and uses Secure, HttpOnly, SameSite cookies (with an `__Host-` name under HTTPS). [CLI admin startup](../../cmd/fry/admin.go) currently binds loopback and calls plain `http.Server.Serve`; public HTTPS therefore requires listener/config wiring. Use `http.Server.ServeTLS` with the loaded pair and TLS 1.2 minimum, retain header/read/write/idle bounds and handler slots, and add raw connection admission if exposing it publicly. Require the configured origin to be HTTPS for public mode. Keep HTTP available only in explicit numeric-loopback fixture mode. Do not treat an HTTPS origin alone as transport encryption, and do not infer TLS from proxy headers. Existing OIDC `LoopbackHTTP` is a separate issuer exception, not permission for public cleartext administration. [Current origin/cookie behavior](../../internal/admintransport/http.go).

## Real TCP verification plan

Use generated private certificate fixtures, ephemeral listeners, isolated database schemas, real clients, and negative raw-protocol scripts:

- SMTP MX: plaintext local delivery still works, unknown recipients fail, AUTH is absent/rejected, STARTTLS succeeds, certificate is verifiable, post-upgrade EHLO is required, TLS 1.0/1.1 fail.
- Submission 587: pre-TLS EHLO omits AUTH; premature AUTH never reaches password verification; plaintext MAIL cannot enqueue; STARTTLS followed by valid authentication enqueues; wrong credentials and unauthorized senders fail. Confirm old session/transaction state does not survive upgrade.
- Submission 465 and IMAP 993: no plaintext greeting; trusted TLS succeeds; wrong root/hostname and old TLS fail; successful auth operates on the correct organization; truncated/stalled handshake closes within its configured bound.
- Optional IMAP 143: LOGIN/AUTHENTICATE fail before TLS, capability changes after STARTTLS, valid authentication works after upgrade, and failed negotiation never resumes cleartext operation.
- Public listener admission: saturate incomplete handshakes, verify capacity is bounded and recovers after close, ensure implicit overload emits no plaintext protocol response, and cancel/shutdown with stalled handshakes plus active ingest/APPEND.
- Admin HTTPS: secure-cookie/origin behavior, valid login/CSRF flows, HTTP-to-TLS failure without credential verification, request/header/connection bounds, and bounded shutdown.
- Retain current plaintext loopback fixtures and explicit rejection of nonloopback binds in `loopback` mode; run race tests and process smoke tests alongside protocol tests.

These tests prove local transport and authorization behavior; they do not establish public DNS, certificate issuance/renewal, mail reputation, abuse controls, Kubernetes exposure, or production capacity.
