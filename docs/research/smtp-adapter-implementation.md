# SMTP reception adapter: implementation findings

Investigated 2026-10-17 against tagged upstream source and actual loopback TCP exchanges. Scope: a local incoming-SMTP adapter over FRY's existing receipt store, not a production MX or submission service. This follows [the foundations](email-gateway-foundations.md), [portable storage investigation](portable-storage-and-imap.md), and ADRs [0002](../adr/0002-direct-and-imap-ingress.md), [0005](../adr/0005-buffer-processing-during-bursts.md), [0007](../adr/0007-outbound-through-existing-relay.md), [0014](../adr/0014-prove-conventional-email-first.md), and [0015](../adr/0015-prefer-replicated-hot-storage-and-packed-objects.md).

## Candidate and provenance

Recommend `github.com/emersion/go-smtp` **v0.25.0**, released 2026-08-13, commit `642c374442498d8e36bc67c27391b5700ac66b89`, with the two explicitly recorded framing fixes below before accepting real messages. The module checksum is `h1:krfiHrme2JbJYDh0DGuSRbvPpbnQTH/v9CIfPincl1I=`. Its inspected cache directory is `/home/coto/go/pkg/mod/github.com/emersion/go-smtp@v0.25.0`. This is a prototype dependency recommendation, not a production suitability finding. [Release](https://github.com/emersion/go-smtp/releases/tag/v0.25.0), [tagged tree](https://github.com/emersion/go-smtp/tree/642c374442498d8e36bc67c27391b5700ac66b89).

The library uses MIT; retain its license and copyright notices in any checked-in copy. Its module requires `go-sasl` at `v0.0.0-20241020182733-b788ff22d5a6`. [License](https://github.com/emersion/go-smtp/blob/v0.25.0/LICENSE), [module](https://github.com/emersion/go-smtp/blob/v0.25.0/go.mod).

Proton's fork has no release or tag returned by GitHub, and its inspected master is `2b3d50599865839dfccb6bf4805996fc3d476f29` from 2023-11-09. Its module still declares the upstream import path. No required FRY capability was identified that justifies this older fork. Gluon remains a separate IMAP investigation. [Proton repository metadata](https://api.github.com/repos/ProtonMail/go-smtp), [tags](https://api.github.com/repos/ProtonMail/go-smtp/tags), [commit](https://github.com/ProtonMail/go-smtp/commit/2b3d50599865839dfccb6bf4805996fc3d476f29), [module](https://github.com/ProtonMail/go-smtp/blob/2b3d50599865839dfccb6bf4805996fc3d476f29/go.mod).

## API contract

`Backend.NewSession(*smtp.Conn) (smtp.Session, error)` creates a session. Its required methods are `Reset()`, `Logout() error`, `Mail(string, *smtp.MailOptions) error`, `Rcpt(string, *smtp.RcptOptions) error`, and `Data(io.Reader) error`. `Data` must consume its input before returning; neither it nor the other methods receives a context. Implementing only `Session`, without `AuthSession`, exposes no AUTH mechanism. [Backend interfaces](https://github.com/emersion/go-smtp/blob/v0.25.0/backend.go).

The server has `Serve(net.Listener) error`, `Close() error`, and `Shutdown(context.Context) error`; limits include message bytes, recipients, line length, read timeout, and write timeout. There is no maximum-connections field. `Shutdown` closes listeners and waits for connections; after it has begun, `Close` returns `ErrServerClosed` without force-closing them. FRY therefore needs its own accepted-connection registry and deadline fallback. Do not assume shutdown alone bounds receipt-worker lifetime. [Server implementation](https://github.com/emersion/go-smtp/blob/v0.25.0/server.go).

`Conn.Conn()` exposes the transport. `Conn.Close` invokes `Logout` under its lock before closing the socket: `Logout` must not call it recursively or wait for a blocked `Data` that needs that close. CHUNKING is always advertised; turning off BINARYMIME does not disable BDAT. BDAT runs `Data` on a goroutine. Ordinary DATA drains remaining input after backend return, removing the size limit during that drain. Bound drain time with transport deadlines. Returning nil produces final `250`; generic errors produce `554` including their text. Return explicit sanitized `*SMTPError` values, such as `451` for a known temporary storage failure. [Connection implementation](https://github.com/emersion/go-smtp/blob/v0.25.0/conn.go).

## Reproduced acceptance-blocking defects

Both reproductions used an unmodified v0.25.0 server on a real loopback TCP listener, `MaxMessageBytes=5`, and a session whose `Data` calls `io.ReadAll`. After successful EHLO, MAIL FROM, and RCPT TO:

| Exchange | Observed backend result | Observed final response |
| --- | --- | --- |
| Send `BDAT 5 LAST\r\nabc`, then TCP `CloseWrite()` | Three bytes `abc`, nil error | `250 2.0.0 OK: queued` |
| Send `DATA`, wait for 354, send `abc\r\n.\r\n` | Five decoded bytes, `ErrDataTooLarge` | `552 5.3.4 Maximum message size exceeded` |

The BDAT implementation ignores the byte count returned by copying a limited reader; premature network EOF can therefore become a clean backend EOF. Minimal fix: retain that copy count and convert a short count with nil error to `io.ErrUnexpectedEOF`, entering the existing failure/reset path. The remaining limited reader preserves correct draining. Test truncated first, intermediate, and LAST chunks, including zero bytes and TCP half-close; assert that no receipt commits. [BDAT implementation](https://github.com/emersion/go-smtp/blob/v0.25.0/conn.go#L993).

The DATA reader checks its exhausted byte budget before recognizing the terminator. Minimal fix: at exactly zero remaining payload bytes, allow a bounded one-byte probe through the dot/terminator parser. A terminator is clean EOF; another decoded payload byte is `ErrDataTooLarge`. Never silently truncate. Test limits minus one, exactly equal, and plus one, dot-stuffed lines, empty messages, and unterminated input. SIZE describes complete decoded email bytes, including encoded MIME attachments and CRLF, rather than attachment bytes alone. [DATA reader](https://github.com/emersion/go-smtp/blob/v0.25.0/data.go), [SIZE specification](https://www.rfc-editor.org/rfc/rfc1870.html#section-2).

**Implementation recommendation:** keep any patched copy isolated under `third_party/go-smtp`, identify its upstream tag and exact patch, retain LICENSE, and run upstream plus FRY framing regressions. Do not hide these protocol changes inside a general network wrapper. The source inspected on upstream master still exhibited both relevant code patterns at research time; no upstream fix was established. No issue or message was sent upstream.

## Proposed local adapter boundary

These are FRY implementation recommendations, not library-provided guarantees:

- Bind loopback by default. Resolve RCPT only against explicitly configured local domains/addresses; unknown recipients receive a permanent recipient rejection. No forwarding or arbitrary outbound relay belongs in this incoming adapter. Defer production public listeners, AUTH submission, TLS policy, and outbound queuing to their own validation steps.
- Capture envelope recipients separately from MIME headers. Deduplicate aliases by destination inbox while retaining accepted address metadata. Since the current receipt transaction belongs to one organization, reject cross-organization recipients in a transaction until atomic fan-out semantics are implemented; make this prototype restriction explicit.
- Allocate a new receipt attempt per SMTP transaction, not per Message-ID. Return success only after the existing durable receipt transaction succeeds. Business processing and packing remain asynchronous. A disconnected sender can retry committed mail, so SMTP is not exactly-once delivery.
- Adapt `Data` to the receipt store's owned `io.ReadCloser` carefully: successful `Accept` also closes it. Once full EOF is observed, Close must preserve the connection for the final response. Before EOF, Close must unblock input; raw connection close or the BDAT pipe reader's Close can do so without recursive session logout. Coordinate Data, Reset, Logout, cancellation, and forced shutdown; never retain the library reader after Data returns.
- An uncertain database commit needs Status reconciliation. If acceptance remains unresolved, close the transport rather than claiming a definite rejection for a potentially committed receipt. If a negative final response is sent, the transaction must not later publish a delivery. [SMTP responsibility and ambiguity](https://www.rfc-editor.org/rfc/rfc5321.html#section-6.1).
- Bound connections, simultaneous receipt attempts, recipient fan-out, message bytes, idle time, total ingestion time, and shutdown. Admission should fail before DATA where feasible; resource exhaustion must never produce successful acceptance without durable ownership. Configure capacity against measured workload later.

## Adapter verification

Use a real loopback listener and the library client: `NewClient(net.Conn)`, `Mail`, `Rcpt`, `Data`, write bytes, then `DataCommand.CloseWithResponse()` to observe the final result. Use raw textproto/TCP exchanges for BDAT, half-closes, pipelining, malformed input, and deterministic lost-response cases. Closing the DATA writer waits for the final server response; a successful payload write alone proves nothing about acceptance. [Client implementation](https://github.com/emersion/go-smtp/blob/v0.25.0/client.go).

Exercise accepted mail against actual PostgreSQL and both hot replicas, compare decoded MIME bytes, prove no final success before durable receipt, and test rejected recipient/relay attempts, alias deduplication, size boundaries, unavailable replicas/database, storage admission failure, reset, partial DATA/BDAT, repeated transactions on one connection, and forced shutdown under the race detector. This research ran only the two isolated upstream reproductions; it does not claim FRY adapter tests, throughput, real SMTP edge operation, or AZ failover have passed.
