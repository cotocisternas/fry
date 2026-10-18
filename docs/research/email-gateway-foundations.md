# FRY email gateway foundations

Research date: 2026-10-17. Scope: first discovery round, using primary sources. These findings support an interview; they do not select FRY's architecture or establish production suitability at FRY's unknown workload.

## Main findings

### 1. Proton's Gluon solves an IMAP problem

Gluon is an IMAP4rev1 server library. Proton introduced it on 2023-02-22 for Proton Mail Bridge, including per-session mailbox snapshots and a connector that synchronizes external message state. Its stated purpose is exposing and managing mail through IMAP. It is not an SMTP receiver, outbound delivery queue, spam filter, or complete MX service. **Implication:** evaluate it if FRY must expose mailboxes to existing IMAP clients; it does not establish the gateway's ingestion architecture. [Gluon README](https://github.com/ProtonMail/gluon), [Proton introduction](https://proton.me/blog/gluon-imap-library).

At inspected commit `2561acfbad82c2ef0f576a8df98b3269a0cbbfee`, the default builder uses SQLite and an on-disk message store. However, `WithDBClient`, `WithStoreBuilder`, and the remote `Connector` are extension points. The message store reads a complete message into `[]byte`, which merits memory testing for large attachments. **Inference:** distributed backing is possible to investigate, but replacing a store does not demonstrate safe multi-node IMAP coordination, failover, or scalable mailbox ownership. Those remain unverified. [Builder](https://github.com/ProtonMail/gluon/blob/2561acfbad82c2ef0f576a8df98b3269a0cbbfee/builder.go), [options](https://github.com/ProtonMail/gluon/blob/2561acfbad82c2ef0f576a8df98b3269a0cbbfee/option.go), [store interface](https://github.com/ProtonMail/gluon/blob/2561acfbad82c2ef0f576a8df98b3269a0cbbfee/store/store.go), [connector](https://github.com/ProtonMail/gluon/blob/2561acfbad82c2ef0f576a8df98b3269a0cbbfee/connector/connector.go).

The repository was unarchived when checked; its default branch was `dev`, whose head commit was dated 2026-10-09. This establishes recent activity, not a support commitment or performance guarantee. Its license is MIT. [Repository metadata](https://api.github.com/repos/ProtonMail/gluon), [inspected commit](https://github.com/ProtonMail/gluon/commit/2561acfbad82c2ef0f576a8df98b3269a0cbbfee), [license](https://github.com/ProtonMail/gluon/blob/2561acfbad82c2ef0f576a8df98b3269a0cbbfee/LICENSE).

### 2. Go has separate SMTP, IMAP, and MIME building blocks

`ProtonMail/go-smtp` and `ProtonMail/go-imap` are forks of the corresponding emersion libraries. Their existence is not evidence that Proton's forks are the best starting point for FRY. Compare required fixes, API versions, and maintenance before choosing a fork. [Proton SMTP fork](https://github.com/ProtonMail/go-smtp), [Proton IMAP fork](https://github.com/ProtonMail/go-imap).

Upstream `emersion/go-smtp` provides ESMTP client/server functionality and LMTP under MIT. Its server backend is an application extension point: FRY would still own durable acceptance, recipient policy, queueing, and processing. Upstream `go-imap` supplies client and server packages; its v2 README currently labels that major version as still in development. An IMAP client for pulling existing mail and an IMAP server for serving legacy consumers are different requirements. [go-smtp](https://github.com/emersion/go-smtp), [go-imap](https://github.com/emersion/go-imap).

`emersion/go-message` provides streaming Internet Message Format/MIME processing, encoding and charset handling, and a mail subpackage, under MIT. This is useful for extracting attachments; MIME decoding does not validate an invoice's business meaning. [go-message](https://github.com/emersion/go-message).

### 3. Existing Go mail servers are useful comparison candidates

Maddy implements SMTP receiving and delivery, IMAP, and mail authentication/security protocols. Its README explicitly calls IMAP storage beta. The repository lists GPL-3.0. Its documented storage separates SQL indexes/metadata from message blobs; supported databases include SQLite, PostgreSQL, and CockroachDB, and its documentation includes an S3 blob backend. These are useful precedents, not proof of FRY-scale distributed operation. [Maddy](https://github.com/foxcpp/maddy), [storage documentation](https://maddy.email/reference/storage/imapsql/).

Mox is a complete Go mail server with SMTP, IMAP, spam filtering, SPF/DKIM/DMARC, multiple domains, and HTTP APIs/webhooks for incoming messages and delivery events. Its README says most non-server Go packages are reusable and identifies MIT licensing, with separately licensed included components. It is a candidate for an operational mail edge or reusable components, but this research has not established a distributed storage model or FRY-scale throughput. [Mox](https://github.com/mjl-/mox).

The license statements above report project files; no conclusion about a particular FRY distribution or embedding arrangement is made.

### 4. Filtering needs separate decisions

Rspamd is a separate daemon that returns a spam score and recommended action. It supports SPF, DKIM, DMARC, ARC, reputation, content rules, and statistical filtering. A Go gateway can use its HTTP protocol, sending message bytes and connection/envelope context such as peer IP, HELO, sender, and recipients. [Rspamd overview](https://docs.rspamd.com/), [protocol](https://docs.rspamd.com/developers/protocol/).

Rspamd's antivirus module integrates scanners such as ClamAV. It distinguishes detections from scan failures; configuration can turn scan failures into temporary rejection. Some scanners also report encrypted content. A successful scan and an unavailable scanner must not be represented as the same result. [Antivirus module](https://docs.rspamd.com/modules/antivirus/).

**Proposed domain separation:** transport/authentication evidence, spam assessment, malware assessment, attachment parsing, and business validation should remain distinguishable. A signed message can carry an irrelevant document; a parseable XML invoice can name the wrong buyer. Business validation therefore needs explicit FRY rules, such as accepted document families, schema/signature checks, counterparties, recipient identity, and duplicate handling. These are requirements to discover, not capabilities supplied automatically by Rspamd.

### 5. SMTP acceptance is a responsibility boundary

RFC 5321 §§4.2.5 and 6.1 assign delivery responsibility after a successful end-of-DATA response. A receiver must not subsequently lose accepted mail because of a crash or predictable resource shortage. A temporary end-of-DATA error leaves responsibility with the sender, and the receiver must not subsequently deliver that rejected transaction. §§4.5.3.2.7 and 6.1 explain that lost/delayed acknowledgments can produce duplicate deliveries. [RFC 5321](https://www.rfc-editor.org/rfc/rfc5321.html#section-6.1).

**Design implication:** before FRY returns final `250`, it needs recoverable message bytes, the SMTP envelope, and enough durable state to resume its responsibility. The RFC does not prescribe PostgreSQL, S3, quorum replication, or a transaction layout. Slow business processing can follow acceptance, but then failure/quarantine handling becomes FRY's responsibility. Delivery attempts and business documents need identities that tolerate retries; the SMTP success response is not an exactly-once business-processing guarantee.

## Storage hypothesis to test

**Proposal, not decision:** retain raw received MIME plus separately captured envelope/ingress evidence in object storage; keep ownership, logical inbox membership, routing, processing attempts, and document references in a relational database. Extracted documents can be derived artifacts linked to the original message. Maddy's documented blob/index separation is a precedent, not a selection of Maddy or its schema.

The important unanswered part is the failure protocol across stores. Define when acceptance is durable, how incomplete uploads or metadata writes are reconciled, how work is retried, and how an accepted message is recovered after worker/node failure. A database transaction cannot by itself atomically commit an unrelated object upload. Storage choice also depends on retention, retrieval/search, maximum attachment sizes, and recovery objectives.

## Requirements that block architecture selection

- Product boundary: integration/document gateway, general mailbox service, or gateway with an IMAP compatibility interface.
- Ingress authority: direct MX, relay from another MTA, polling existing IMAP accounts, or a migration sequence combining these.
- Meaning of existing “mboxes”: actual mbox files, mailboxes generally, provider quotas, inode counts, backup duration, or search/performance problems.
- Ownership: one business with several domains versus independently isolated customer organizations; relationship between domain, address, inbox, and processing destination.
- Filtering outcomes: synchronous rejection, temporary rejection, quarantine, or accepted delivery; who reviews false positives and what constitutes a valid document.
- Outbound scope: relay through an existing provider versus operating internet delivery, retries, and failure notifications.
- Measured load: messages/day and burst rate, attachment distribution and maximum size, inbox/domain counts, retention, processing latency, and tolerated loss/downtime.
- Consumer contract: IMAP access, webhook/API/queue delivery, manual review, replay, and acknowledgment semantics.

No performance benchmark, failover test, dependency audit, or complete library comparison was performed in this first round.
