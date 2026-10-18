# Local mailbox and IMAP evidence

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

The prototype now provides conventional shared mailbox state in PostgreSQL and a loopback IMAP endpoint using pinned `go-imap/v2 v2.0.0-beta.8` with isolated [framing and writer extensions](../../third_party/go-imap/FRY-PATCHES.md). This is a compatibility subset, not completion of the conventional-email production milestone.

## Durable mailbox contract

A stable inbox belongs to one organization. Folders have persistent UIDVALIDITY and monotonically increasing UIDs, distinct from immutable receipt identities. Recreating a deleted folder assigns a new UIDVALIDITY; ordinary restart and rename into an unused name preserve it. UID exhaustion fails explicitly rather than wrapping.

Mailbox publication consumes the receipt's durable work item. Membership creation, UID allocation, visible-mail ownership, and release of pending ownership commit together. Concurrent workers and replay cannot create duplicate memberships. Expunge releases only that membership's ownership and never restarts the receipt-based recovery clock. COPY adds a new membership/reference; MOVE transfers ownership atomically. Deleting a folder releases its memberships while preserving copies in other folders.

The database tests cover concurrent publication, organization isolation, visible mail surviving recovery expiry, flag changes, selective expunge, copy/move/reference preservation, stable identifiers, and recreation with a fresh UIDVALIDITY. These operations share receipt/folder guards with the storage lifecycle.

## Protocol evidence

Real TCP tests cover:

- Authentication, SELECT/EXAMINE, persistent UID/flags across endpoint restart.
- Complete raw messages and clipped partial FETCH, including a range beyond EOF. The full suite also uploads a 200 MiB message through APPEND and streams it back into a client-side hash without collecting the body in memory; byte count and digest match. This is not an RSS or throughput benchmark.
- Header/date/flag SEARCH, with unsupported criteria rejected explicitly.
- Shared clients, STORE, EXPUNGE, and sequence-number preservation until the client receives the expunge notification.
- Read-only access preventing flag changes and implicit Seen updates.
- COPY/MOVE UID mapping including the same folder and empty UID selections, folder creation with trailing hierarchy delimiters, subtree/INBOX rename, and deletion.
- ENVELOPE with decoded display names/subjects, address groups, and Sender/Reply-To fallback. Header SEARCH decodes queried RFC 2047 fields, including nested OR/NOT criteria; unavailable charsets fail explicitly.
- BODY/BODYSTRUCTURE, nested multipart and message/rfc822 sections, MIME headers, exact encoded attachment bytes, and partial ranges. Malformed MIME leaves raw FETCH available.
- Top-level HEADER.FIELDS/NOT, preserving folded/repeated fields and applying partial ranges after filtering.
- Explicit failure for header-dependent operations exceeding the parsing bound.
- Atomic APPEND receipt/membership/UID publication, initial flags/date, stable acknowledgment after expunge, and no deferred publication after a forced database failure.
- Truncated literals, missing/malformed command trailers, and invalid empty APPEND commands create no accepted receipts.

`./scripts/tests/receipts.sh -race -short` passes with these tests. `./scripts/smoke.sh smtp` builds the actual executable, accepts messages through SMTP, retrieves identical bytes through Python's IMAP client, restarts against the same storage, and checks both old and new mail. This path uses real PostgreSQL and local files; it does not use AWS.

## Current limits

The endpoint supports numeric-loopback fixture access and explicit [public implicit TLS](public-mail-transport.md). The executable uses [persistent administrator-managed IMAP credentials](administration.md), seeded from startup configuration without overwriting later changes. Each operation that accesses mailbox state checks the credential generation. Static accounts remain available for protocol fixtures. Public TLS has local protocol and process coverage; deployment and actual integration-client validation remain outstanding.

Body/text search and BINARY decoding remain outstanding. Unsupported operations return an explicit error. Header-dependent operations stop at the header boundary and have a 1 MiB parsing limit and fail explicitly on malformed/oversized headers; full raw retrieval is independent of that limit. Recent flags are not tracked.

Each selected session binds immutable folder identity plus UIDVALIDITY and retains an ordered UID snapshot. Missing members retain their sequence slots until EXPUNGE is permitted; changes are reconciled from PostgreSQL at command boundaries or during IDLE. This avoids using current database row positions for another client's old sequence numbers. Folder scans and session snapshots currently scale with mailbox size; indexing, paging, bounded metadata caches, notifications, and production load testing are outstanding. This is not a distributed mailbox-owner/failover implementation.

Publication uses a bounded polling worker with [rotating organization turns](mailbox-fairness.md). Each call selects one organization, advances its process-local cursor before publication, and applies individual destination transactions; this supplies opportunity fairness without a latency guarantee. Failed destinations retain pending ownership and receive a durable retry delay while other batch items continue. [External IMAP collection](imap-collection.md) has since been implemented locally. Fresh [schema initialization](schema-installation.md) now installs all components and a version ledger atomically; startup rejects incompatible installations, and upgrades remain unsupported. [Staged history migration](mailbox-migration.md) now has local protocol/process coverage; representative cutover timing, production restart/upgrades, and actual legacy-client compatibility remain separate work. [Local outbound relay queues](outbound-relay.md) have since been added.

## APPEND durability and framing

The [upstream investigation](../research/imap-compatibility-extension.md) reproduced premature success at the backend reader for a truncated literal and for a complete payload followed by an invalid command trailer. The isolated local library patch withholds the final payload chunk until the exact literal and trailer are valid, checks empty literals before calling the backend, preserves next-command bytes, and makes errors sticky. This also protects backends that use `io.CopyN`, which can otherwise discard an error on a full final read. Nested-module tests exercise both ReadAll and CopyN over actual TCP and preserve the upstream MIT license/provenance.

FRY then uses `Envelope.MailboxAppend` to publish a single explicitly named folder membership within the receipt acceptance transaction. It does not enqueue background mailbox publication. UID allocation, initial flags/date, and ownership transfer roll back if acceptance fails. An immutable acknowledgment row preserves the original UID/UIDVALIDITY even if another session later moves/expunges the message. Unknown commit results are resolved by attempt identity; if success cannot be established or the acknowledgment cannot be read after acceptance, the connection closes instead of reporting a definite rejection. A client repeating APPEND after losing its successful reply can still create a duplicate.

The wire APPEND limit is set to the executable's configured message limit, bounded at 256 MiB; storage admission remains authoritative. Optional parser/metadata errors do not prevent raw APPEND or raw retrieval. Current APPEND operations inherit the local adapter's 30-second operation deadline, which is an explicit prototype limitation for slow large uploads. Standard flags and ASCII keyword flags are supported; Recent semantics remain separate compatibility work.

## Bounded MIME indexing

`internal/messageindex` streams the original message into a bounded tree of header/body byte spans. It excludes the newline owned by each enclosing MIME delimiter, preserves preamble/epilogue in enclosing sections, supports multipart/digest defaults and unencoded encapsulated messages, and never decodes attachment bytes. BODY sections stream those original spans from retained storage. All requested sections and BODYSTRUCTURE metadata are validated before starting that message's FETCH response.

Current limits are 1 MiB per header block, 8 MiB aggregate headers, 1,024 fields per block / 8,192 aggregate, 1,024 parts, depth 32, 70-byte MIME boundaries, and 128 parameters per parsed MIME field. Four MIME FETCH commands may hold indexes concurrently per endpoint, within the existing 30-second operation deadline. Body scanning uses a 32 KiB buffer and a 256-byte line prefix even for an unbroken 200 MiB attachment. The isolated allocation regression checks that scanning that attachment allocates less than 2 MiB; this is not a whole-server RSS or concurrency benchmark. The large TCP test uploads and retrieves a 200 MiB message and verifies its BODYSTRUCTURE size plus a clipped attachment range.

Each MIME-dependent FETCH currently scans the message again. A durable index/cache, scan deduplication, tenant fairness, and load characterization are still outstanding; this is not yet an efficient production path for repeated attachment queries. Raw/top-level header access avoids that MIME scan. Malformed, over-budget, or transfer-encoded containers fail metadata access explicitly without rejecting original mail admission. BINARY, encoded encapsulated-message traversal, and message/global BODYSTRUCTURE remain unsupported. Nonexistent sections return NIL through the isolated writer extension, while existing empty sections and beyond-end partials return empty literals. The remaining gaps above must close before claiming a complete IMAP server.

## Keyword flags

APPEND, STORE, SEARCH, COPY/MOVE, and external collection preserve custom ASCII keyword flags. Keyword comparison is case insensitive; storage uses lowercase canonical names while registered keywords may be rendered with conventional casing by the protocol library. System flags retain their canonical forms. Explicit Recent/unknown system flags cannot be persisted. SELECT advertises keyword creation with PERMANENTFLAGS \*, and selected sessions receive updates to the folder's available keyword names.

Prototype budgets are 128 flags per message, 128 bytes per keyword, and 256 distinct keyword names per folder over that folder's lifetime. Names remain in the folder catalog after their last message flag is removed, avoiding unstable SELECT metadata. Catalog and message changes share the folder guard and transaction; hitting either limit rolls back the entire mutation. Existing messages remain accessible. These are bounded compatibility defaults, not measured production sizing. A collected message waiting on a full destination catalog remains pending publication rather than losing its original bytes.

## Folder identity and namespace

Selected FETCH/SEARCH/STORE/COPY/MOVE/EXPUNGE operations use a `MailboxSelection` bound to folder ID and UIDVALIDITY through their database guards. Resolving its old name again is unsafe: another client may rename the folder and create a replacement under that name. Real two-client tests verify the selection continues accessing the original identity and never mutates a replacement after rename or deletion. A deleted identity or changed UIDVALIDITY fails selected operations until the client selects again; FRY does not silently rebind it.

Ordinary RENAME atomically moves a folder and its descendants, including a virtual parent represented by children. It creates missing destination ancestors, checks collisions and name bounds, and rejects moving a tree inside itself. Percent and underscore bytes in actual names are not SQL wildcards. Virtual hierarchy entries appear as Noselect in LIST. INBOX RENAME uses a separate atomic transaction: all current messages transfer with their dates/flags/references to a fresh destination, INBOX retains its identity and increasing UIDNEXT, and INBOX children remain unchanged. An unguarded arrival discovered during that transfer causes retry rather than partial movement. This uses snapshots and namespace/folder locks; it has not been benchmarked on a concentrated production mailbox.

Subscriptions now persist by inbox/name independently of live folders. Shared mailbox credentials share these subscriptions, preserving the previous sharing scope. Rename leaves old-name subscriptions intact; deletion does not remove them; UNSUBSCRIBE works after deletion and when repeated. Previously subscribed destination names remain subscribed when reused. Legacy LSUB percent-wildcard parent synthesis and empty-pattern LIST hierarchy discovery are covered by TCP tests. The isolated `ListWriter.IsLSUB` extension distinguishes those legacy rules from LIST (SUBSCRIBED). Unimplemented extended LIST options fail explicitly.

Historical name records preserve the previous UID namespace after rename or deletion. A newly created folder gets a globally increasing UIDVALIDITY. Renaming an older folder onto a historically newer name allocates a newer UIDVALIDITY while preserving message UIDs; stale selections cannot mutate that new namespace. Other ordinary renames preserve IDs, UIDs, and UIDVALIDITY. Subscriptions and historical names need bounded administration/retirement policies before production.

Same-folder COPY creates new UIDs. Same-folder MOVE creates new UIDs and expunges only its originals, preserving unrelated Deleted messages. Empty UID sets are successful no-ops without malformed empty COPYUID responses. EXAMINE still blocks selected-source mutation, while independently authorized destination COPY/APPEND remains possible. See [primary-source findings](../research/imap-folder-semantics.md).

## Snapshot contention and expunge work

Mailbox reads now use one read-only repeatable-read PostgreSQL transaction for both folder metadata and ordered message membership. They no longer hold the folder row lock used by UID publication while materializing the mailbox. Mutating operations retain their receipt/folder locks and revalidation. The [snapshot tests](../../internal/receiptstore/mailbox_snapshot_linux_test.go) first reproduced blocked readers behind an uncommitted writer, then verified coherent prior committed reads, concurrent publication/flag/expunge snapshots, immutable namespace identity, organization and migration scope, and transaction release on cancellation. These focused tests passed with race detection.

Poll, EXPUNGE and MOVE now share a stable linear compaction helper instead of shifting the entire remaining slice after every removal. [Wire and helper tests](../../internal/imapserver/compaction_test.go) preserve consecutive/interleaved EXPUNGE sequence numbers, deferred tombstones, flag updates, and successful-prefix state after an output failure. Removed trailing references are cleared. The bounded algorithm benchmark (including its reset copy) measured about 11.0 µs, 117.2 µs and 1.46 ms at 1,000, 10,000 and 100,000 slots respectively, with zero allocations per operation on the shared Intel Core Ultra 9 workstation. It excludes PostgreSQL, message reads and network output; it does not measure mailbox capacity.

These changes remove lock coupling and quadratic slice work. Full folder scans, selected-session slices and UID maps still grow with retained messages, and IDLE still polls. Bounded snapshot design and representative 500,000-message mailbox validation remain outstanding; [publication batching was evaluated and rejected](mailbox-publication.md) after intake regressions. [Receipt-governed header metadata](local-load.md) improves repeated header queries while preserving original-byte and cleanup authority. [Streaming header capture](header-capture.md) can provide that metadata at acceptance; lazy-fallback receipts still require first-query extraction and persistence.

The combined `./scripts/tests/receipts.sh -race -short` suite and `go vet ./...` passed after the snapshot and compaction changes. This includes receipt, migration, administration and IMAP protocol regressions; no additional load-capacity claim is inferred.
