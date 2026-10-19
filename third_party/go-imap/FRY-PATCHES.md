# Isolated FRY compatibility patch

Upstream: `github.com/emersion/go-imap/v2 v2.0.0-beta.8`, commit `7ac47a9cfd9a06ff5f3bdee52d7420137f023a1e`. Copied from the Go module cache with its original MIT license. FRY's root module uses a local replacement; upstream version provenance remains pinned. This is not an upstream release or an upstream-submitted fix.

`imapserver/append.go` wraps APPEND's literal reader so a backend cannot observe the full payload before its command trailer is valid. It converts early EOF to `io.ErrUnexpectedEOF`, withholds the final payload chunk until CRLF/optional UTF8 closing-parenthesis validation succeeds, and checks zero-byte literals before invoking the backend. Returning zero bytes on the failed final read is necessary because `io.CopyN` can otherwise discard an error after receiving the requested count. Handler draining uses the same reader, propagates its actual error, and no longer decodes the trailer twice.

`imapserver/fry_append_test.go` covers ordinary reads and CopyN, sticky terminal state, pipelined next-command preservation, empty/truncated/malformed literals and UTF8 framing. Actual TCP tests send payload bytes individually and verify that invalid commands never reach successful backend completion. FRY's own integration tests additionally verify no receipt/mailbox publication for malformed commands and atomic mailbox publication on success.

The research/reproduction is in `docs/research/imap-compatibility-extension.md` at the repository root. Keep this patch isolated when rebasing upstream. Root `go test ./...` does not execute nested-module tests; run:

```sh
go -C third_party/go-imap test -race ./...
```

`imapserver/fetch.go` adds `WriteBodySectionNil` so FRY can distinguish a nonexistent MIME section (`NIL`) from an existing empty section (a zero-length literal). `fry-fetch-nil.patch` records this independent extension. FRY's real TCP test `TestIMAPAbsentSectionIsNilAndEmptySectionIsLiteral` exercises both responses through the pinned client decoder.

`imapserver/list.go` adds the read-only `ListWriter.IsLSUB` accessor. The backend needs to distinguish legacy LSUB's percent-wildcard parent synthesis from LIST (SUBSCRIBED); the existing ListOptions values alone do not distinguish those commands. `fry-list-command.patch` isolates this extension. FRY's TCP test checks retained subscriptions, virtual parents, legacy LSUB, and the empty-pattern LIST separator response.

`imapclient.Options.StrictFetchRouting` is an opt-in for FRY's serialized,
identity-verifying history reader. It routes all FETCH responses to the pending
FETCH command even when a literal precedes UID; the caller must validate UID,
reject additional responses, and never pipeline FETCH commands. Default upstream
routing is unchanged. The option intentionally exposes unsolicited/duplicate
responses for rejection. `fetchLiteralReader` also retains its terminal error,
so cleanup cannot re-read the decoder concurrently after a truncated literal has
released the protocol reader. `imapclient/fry_fetch_routing_test.go` covers the
trailing UID over a real pipe; root `internal/imapsource` TCP tests additionally
cover tagged failure, truncation, duplicate literals, and mismatched identities.

`Options.RejectDuplicateSearchResults` rejects duplicate numbers in legacy SEARCH
before AddNum normalizes its set. ESEARCH set parsing remains the underlying
parser's responsibility; FRY validates returned ranges, overlap and cardinality
before expanding them. Neither option imposes a parser allocation or line limit.

`Client.LSub` exposes baseline LSUB via ListCommand. LIST and LSUB responses
route only to the matching command kind. FRY combines bounded LIST and LSUB,
preserving subscriptions of selectable folders; nonexistent subscriptions and
LSUB virtual parents are intentionally omitted. Root source TCP tests exercise
this baseline path without requiring LIST-EXTENDED advertisement.

`fry-history-reader.patch` records the client changes above against the pinned upstream module, including the baseline LSUB encoder/response routing. Keep the companion protocol tests when rebasing.

`fry-command-completion.patch` publishes client command results after
continuation cancellation, connection-state updates, and result-stream closure.
It also invalidates cached capabilities before publishing successful LOGIN,
AUTHENTICATE, UNAUTHENTICATE, and STARTTLS results without a CAPABILITY response
code. The command state changes before the invalidation launches its asynchronous
CAPABILITY writer. Logout remains terminal if that writer, or another concurrent
writer, closes the connection before a detached tagged command completes.
Previously `Authenticate` could return success while `State()` still reported
not authenticated, and these commands could return with stale `Caps()`.
`imapclient/fry_command_completion_test.go` stops completion at its result
notification to check state, mailbox, LIST/FETCH/EXPUNGE streams, and the tagged
response capability state, including rejected commands and a CAPABILITY response
code. A full LIST buffer is drained through `Collect()` to check that publishing
last does not block a valid consumer. The STARTTLS test checks that the queued
capability refresh waits for the encoder lock held through the TLS handshake.
The controlled refresh-write failure test closes the client before the detached
LOGIN, AUTHENTICATE, UNAUTHENTICATE, SELECT, and UNSELECT completions and checks
that none restores an open state.
The existing TCP `TestClient_Authenticate` and `TestStartTLS` exercise the public
paths. Apply this patch after the other client patches when reconstructing the
pinned fork. It has zero context lines so the stored patch passes whitespace
checks; use `patch -p1` or `git apply --unidiff-zero` to apply it.

`fry-shutdown.patch` registers listener wait-group ownership under the same mutex
as the closed-state transition. This prevents concurrent Serve/Close from
starting a positive Add after Close began waiting. A connection goroutine that
starts after the closed transition exits before registering or creating a
backend session. `imapserver/fry_shutdown_test.go` exercises concurrent startup
and shutdown under the race detector and rejection of a delayed connection.

`fry-keyed-headers.patch` names the embedded `Header` field when constructing
`go-message` and `mail` headers. This removes the full-module `go vet` composite
literal findings without suppressing that analyzer or changing MIME behavior.
The patch covers server message extraction and the in-memory example backend;
keep it separate from FRY's protocol changes when rebasing upstream.
