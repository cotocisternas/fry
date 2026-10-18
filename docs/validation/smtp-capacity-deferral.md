# SMTP deferral before reading DATA

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Status: repair implemented; focused/full race checks and the real-process
rehearsal passed on 2026-10-17.

The automatic-rotation process rehearsal exposed a protocol problem at full
bounded storage: 69 retry attempts received connection resets rather than
explicit temporary replies. Its authoritative fixture checks proved that those
specific attempts were not accepted. A real SMTP client cannot infer that from
a reset alone; an interrupted final reply after successful acceptance remains
ambiguous.

The independent regression constructs actual one-record bounded members,
accepts an original over SMTP, and submits another complete body to the full
pair. It checks authoritative nonacceptance, expects `451`, requires a usable
`NOOP`, rotates both members normally, and accepts another original on the same
connection. It does not replace storage acceptance with a stub or modify
capacity rows to force the failure.

The unchanged adapter failed this regression in session `77507`, with
`connection reset by peer` instead of `451` after `0.17s`. Command:

```sh
./scripts/tests/receipts.sh -race -short -run '^TestSMTPPrereadCapacityRejectionReturns451AndPreservesSession$'
```

Evidence: `/tmp/fry-smtp-preread-red.log`. The nonacceptance checks passed before
the response assertion failed. The source path explains the result:
`Store.Accept` always closes its input, including preparation failure before
reading; the adapter closes an incomplete DATA socket, preventing the protocol
library from draining framing and sending the intended temporary reply.

## Implemented repair

The repair is restricted to a DATA source whose first read has not started.
A mutex linearizes the first read against source closure: closing an untouched
DATA source marks it closed and defers socket closure; a later source read
returns `io.ErrClosedPipe` without starting protocol I/O. If a read has already
started, closure still interrupts the raw connection. BDAT continues to close
its pipe immediately and use the existing chunk handler.

The adapter resolves uncertain acceptance and confirms terminal nonacceptance
through the existing status/abort path before draining. It then reads the
original DATA protocol reader, rather than reopening the closed source wrapper,
with a fixed 32 KiB buffer and a maximum of `MaxMessageBytes + 1` decoded bytes.
Only the protocol reader's actual EOF, within the configured byte limit and
with the ingest context still live, permits the final negative reply. The
existing ingest cancellation callback remains active throughout this drain.
Oversized, truncated, stalled, canceled or otherwise incomplete input closes
the connection.

A finalizer runs before ingest cleanup on every exit. An untouched accepted
replay or an unresolved outcome without framing EOF therefore disconnects;
it cannot become a definite rejection or leave the library draining after the
ingest deadline has been released. Already consumed originals retain their
existing success and ambiguity-resolution behavior. Receiptstore input
ownership and blocked-read cancellation were not relaxed.

Implementation and tests:

- [SMTP source and DATA handling](../../internal/smtpserver/server.go).
- [Source ownership and drain tests](../../internal/smtpserver/preread_source_test.go).
- [Independent real-capacity wire tests](../../internal/smtpserver/preread_rejection_test.go).

## Focused validation

Focused PostgreSQL-backed race run `30887` passed: SMTP `7.489s`, log
`/tmp/fry-smtp-preread-focused.log`. It includes the previously failing real
bounded-capacity regression: the rejected body receives `451`, `NOOP` succeeds,
and after explicit rotation the same connection accepts another original.

Additional wire cases exercise oversized, truncated and stalled DATA, shutdown
during drain, and an accepted replay whose replacement DATA remains unread.
They require termination without a false acknowledgement or a reusable negative
reply lacking framing proof. Source tests cover both first-read/close orderings,
blocked-read interruption, unchanged BDAT closure, exact-size EOF, oversize,
no-progress and canceled input, and the finalizer for unresolved or accepted
untouched sources. Injected source cases complement rather than replace the
actual bounded-storage regression.

The complete `./scripts/tests/receipts.sh -race -short` run passed, session
`39625`: receiptstore `102.002s`, SMTP `45.382s`
(`/tmp/fry-smtp-preread-race.log`). `go vet ./...` also passed
(`/tmp/fry-smtp-preread-vet.log`). The short suite excludes the separate 200 MiB
stress case. Independent source and wire-test review found no blocker.

The fresh automatic-rotation process rehearsal passed, session `82571`
(`/tmp/fry-smtp-preread-runtime.log`). It now explicitly requires `451` at the
full-file rejection before rotation is enabled. All 69 unsuccessful attempts
received explicit temporary replies, with zero disconnects. Twelve originals
were accepted, four rotations completed, and every exact original was verified
through each of two independent replicas. Final lane accounting, clean owner
closure and checked cleanup passed. The earlier 69-reset result remains in
[automatic rotation validation](automatic-hot-rotation.md) as historical evidence.

Validated source/binary hashes:

```text
internal/smtpserver/server.go   edf6555165c0e12305148e216f1caf662019ef5d5e99ef6839bbfc63ebb895b1
scripts/smoke/automatic_rotation.py  3baf536baac2551cca048944cf0d3e1d5cd01dac9eb9abbd45ee255a108a65f3
fry binary                     1789759c3e88f83b90cdb40acc726134411b37088e38baaf4a19df9e803e736c
fry-member binary              5db71f8690186145a22d714d0b70ddd8022d8232a7e6c2b3e154b615f1d51ca7
```

These rapid synthetic retries are a recovery probe. No production load,
SMTP retry policy or exactly-once delivery claim follows from these tests.
