# FRY's isolated go-smtp copy

Upstream: github.com/emersion/go-smtp v0.25.0, commit
`642c374442498d8e36bc67c27391b5700ac66b89`.
Original module checksum: `h1:krfiHrme2JbJYDh0DGuSRbvPpbnQTH/v9CIfPincl1I=`.
The upstream MIT [LICENSE](LICENSE) and source notices are retained.

Two local changes are necessary before FRY can trust a completed input stream:

1. In `conn.go`, check the number of bytes copied from a declared BDAT chunk.
   A short copy with nil error becomes `io.ErrUnexpectedEOF` and follows the
   existing failure/reset path. The backend receives `ErrDataReset`, preserving
   the existing cancellation contract. Truncated LAST chunks cannot become
   clean EOF and successful acceptance.
2. In `data.go`, enforce the decoded payload limit only when the parser would
   emit a payload byte. Parsing the terminator after exactly the allowed number
   of bytes succeeds. An additional decoded byte is rejected, never emitted.

`fry-framing.patch` records the exact diff from the pinned source. No issue,
patch or message has been sent upstream. Revisit this replacement when upstream
ships equivalent fixes, and run both its tests and FRY's real-TCP regressions.

```sh
go -C third_party/go-smtp test ./...
./scripts/tests/receipts.sh -short
./scripts/tests/receipts.sh -race -short
```

FRY tests cover short first/LAST/later chunks and half-close, empty LAST chunks,
DATA size boundaries, raw-byte preservation and durable acceptance. See
[the research report](../../docs/research/smtp-adapter-implementation.md) for
source links, reproductions and additional lifecycle limitations.

Public transport additions: `Server.TLSHandshakeTimeout` optionally bounds both
implicit and STARTTLS handshakes with `HandshakeContext`; zero preserves the
upstream timeout behavior. `Server.RequireTLS` rejects all commands except EHLO,
NOOP, STARTTLS and QUIT before TLS, before SASL initial-response decoding or mail
handling. It is for authenticated submission and must not be used for public MX.
A failed STARTTLS handshake now closes the connection immediately rather than
returning to the plaintext SMTP command loop. Existing successful-upgrade state
reset remains unchanged. FRY real TCP tests in
`internal/smtpserver/transport_test.go` cover both TLS modes, AUTH and MAIL gating,
optional incoming TLS, failed negotiation, admission limits, stalled handshakes,
and cancellation. Keep the transport patch isolated on upstream rebase.

Shutdown/admission correction: listener registration, accepted-connection
registration, WaitGroup.Add, and the closed-state transition now share the server
mutex. A Serve call after shutdown closes its listener; an Accept completing
late closes its socket without starting a handler. This prevents Add racing a
zero-counter Wait and handlers registering after Close's connection scan.

BDAT teardown correction: asynchronous Data captures its session, result channel,
status collector and recipient list before launch. Close retains the session
reference while logging it out once, and pipe pointers are captured under the
connection lock. Reset and protocol-handler teardown join the Data goroutine
before reuse/completion, so shutdown cannot lose a still-running backend or let
it dereference a cleared session. `fry_shutdown_test.go` deterministically covers
late Accept, Serve after shutdown, reset joining, and close during backend Data.
