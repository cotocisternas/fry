# Public mail transport validation

Implemented and checked locally on 2026-10-17. FRY now supports explicit TLS modes for incoming SMTP, authenticated submission, IMAP, and direct administration HTTPS. The checks use generated private certificate fixtures and real TCP connections. They do not establish a deployed public mail service or AWS/Kubernetes readiness.

## Configuration and behavior

[examples/public-mail.json](../../examples/public-mail.json) shows all listeners with numeric public bind addresses, placeholder PEM paths, example domains, and environment variable names for passwords. Replace these values before running it. The example retains local replica members; their different labels do not prove independent production failure domains. It does not configure AWS. Existing [local-mail](../../examples/local-mail.json) and [local-admin](../../examples/local-admin.json) examples retain plaintext loopback behavior.

Listener fields are flat in the relevant JSON object: `transport`, `tls_cert_file`, `tls_key_file`, and `handshake_timeout_seconds`, alongside `listen`. Incoming SMTP uses the root object; other listeners use `imap`, `outbound.submission`, and `admin`. Port numbers do not choose transport.

| Listener | Example | Verified behavior |
| --- | --- | --- |
| Incoming SMTP | `0.0.0.0:25`, `starttls` | STARTTLS advertised; plaintext local delivery remains permitted; no AUTH; configured recipients only |
| Submission | `0.0.0.0:587`, `starttls-required` | AUTH hidden before TLS; premature AUTH/MAIL rejected; authentication and authorized sender required after TLS |
| Submission alternative | `0.0.0.0:465`, `tls` | TLS before greeting; authentication and authorized sender required |
| IMAP | `0.0.0.0:993`, `tls` | TLS before greeting and password authentication |
| Administration | `0.0.0.0:8443`, `tls` | Direct HTTPS, HTTPS origin, Secure session cookies and existing authorization/CSRF behavior |
| Existing fixtures | numeric loopback, omitted transport or `loopback` | Plaintext allowed only on loopback, with no TLS settings |

There is currently **one submission listener per process**. To select 465, replace `outbound.submission.listen` with `0.0.0.0:465` and its `transport` with `tls`; do not add a second submission object. Simultaneous 587/465 listeners are not implemented. The CLI does not expose IMAP STARTTLS on 143. Incoming SMTP also accepts implicit `tls` mode when explicitly configured, but the public MX example uses opportunistic STARTTLS on 25.

`prepareListeners` validates every listener before database initialization or background workers start. It requires numeric IP/port binds, loads certificate/key pairs once, sets a TLS 1.2 minimum, defaults handshake timeout to 10 seconds, and accepts configured timeouts from 1 through 60 seconds. `loopback` rejects nonloopback addresses, certificate fields, and handshake settings. Administration `tls` requires an HTTPS origin; loopback administration requires an HTTP numeric-loopback origin. Certificates must contain the names clients use and be trusted by those clients. Certificate acquisition, automatic renewal, and dynamic reload are not implemented; replace files and restart through the deployment's operational procedure. [Root validation](../../cmd/fry/transport.go).

The example's `.test` domains are placeholders: it does not create DNS or issue certificates. Password fields reference environment variables (`FRY_IMAP_PASSWORD`, `FRY_SUBMISSION_PASSWORD`, `FRY_RELAY_PASSWORD`, `FRY_ADMIN_PASSWORD`); it contains no password values. The configured relay remains an explicit authenticated, verified-TLS relay, with no plaintext fallback. Public recipient routing remains explicit and organization scoped. Every nonloopback submission listener now requires a live sender grant from DNS controller verification or a reasoned global override, in addition to the account's static address/domain restriction. The executable wires receiptstore authority automatically; the public example cannot send merely because its static account lists contain an address. See [outgoing sender authority](outgoing-sender-authority.md) and [relay validation](outbound-relay.md).

## Bounds and authentication

SMTP tracks raw sockets before wrapping them in TLS, so incomplete handshakes consume the same bounded connection budget as established sessions. The protocol library receives a visible `*tls.Conn`; implicit saturation closes without a plaintext SMTP response. A separate handshake timeout covers both implicit TLS and STARTTLS. Failed STARTTLS closes the connection. Submission rejects commands before TLS except EHLO, NOOP, STARTTLS, and QUIT; password verification and mail acceptance cannot occur on that cleartext path. [SMTP transport tests](../../internal/smtpserver/transport_test.go).

IMAP bounds pending handshakes and total connections and closes stalled sockets during timeout/shutdown. Administration admission bounds raw connections before HTTP/TLS handling, including stalled handshakes; existing HTTP header, request, idle, and handler limits remain in effect. There is no proxy trust mode, forwarded-header TLS inference, or reliance on an external TLS terminator in this configuration. [IMAP transport tests](../../internal/imapserver/transport_test.go), [HTTP listener bounds](../../cmd/fry/transport.go).

Clients must negotiate TLS before sending credentials. Servers cannot prevent a broken client from transmitting unsolicited plaintext secrets; they can avoid advertising or accepting cleartext authentication, invoking password verification, or logging protocol credentials. Protocol debug tracing remains disabled.

## Checks and evidence

The following focused race checks passed during this work:

```sh
go test -race ./cmd/fry \
  -run 'Test(ListenerConfiguration|AdministrationHTTPS|AdministrationAdmission)' -count=1

FRY_TEST_DATABASE_URL='<disposable PostgreSQL URL>' \
  go test -race ./internal/smtpserver ./internal/imapserver \
  -run 'Test(SubmissionTLSModes|IncomingOptionalTLS|PlaintextAuth|Implicit|InvalidTransport|IMAPTransport)' \
  -count=1 -timeout=90s

go -C third_party/go-smtp test -race ./... -timeout=60s
```

SMTP tests exercise 587/465 authentication and sender restrictions, incoming plaintext delivery/STARTTLS advertisement, malformed pre-TLS AUTH, failed upgrade closure, wrong certificate roots/names, old TLS rejection, handshake admission/timeout/recovery, and shutdown. IMAP checks cover trusted TLS login, certificate rejection, handshake and connection capacity, shutdown cancellation, and public plaintext rejection. Root command tests cover startup validation plus real administration HTTPS, handshake timeout, connection saturation/recovery, and cancellation.

The administration admission regression also controls the interval between closing
the underlying TCP socket and releasing its connection slot. Peer EOF alone is
not a server-side release barrier. Recovery assertions wait for server `Close`
completion; the stalled-handshake test separately requires server closure before
the client deadline. Repeated race checks passed without changing production
connection limits or timeout budgets.

The complete PostgreSQL-backed repository race suite passed with transport, relay binding, and sender authority included. Both vendored mail-library race suites also passed after correcting startup/shutdown registration races. The separate-process test exercised SMTP STARTTLS, IMAP TLS, HTTPS login and secure cookies, sender approval/revocation, and queued relay handoff. Tests against ephemeral loopback ports exercise public-mode protocol behavior without claiming public-network reachability.

## Remaining production gates

This change establishes transport options, not a production readiness certification. DNS/MX and relay configuration, real certificate issuance/renewal, representative capacity, abuse/rate controls, public ingress filtering, monitoring, recovery procedures, HA/failure-domain deployment, and public Kubernetes exposure remain deployment work or unverified gates. Public submission now requires the separately documented DNS-controller grant or global override; static sender/domain restrictions remain bootstrap configuration and an additional limit. From/Sender header authorization, null-sender submission policy, periodic reverification, and dynamic submission-account provisioning remain unfinished. Existing configured accounts support [persistent password rotation and disablement](submission-credentials.md). Admin deployment still needs appropriate identity/access policy; enabling HTTPS does not grant that policy.

Earlier [transport research](../research/public-mail-transport.md) records the proposal and findings; this validation note and current configuration describe implemented scope. In particular, simultaneous submission ports and IMAP 143 STARTTLS remain proposals, not shipped CLI features. The earlier loopback-only descriptions in historical validation documents describe their original test scope and should not override these explicit mode rules.
