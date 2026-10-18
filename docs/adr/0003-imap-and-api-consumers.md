# Preserve IMAP consumers and offer API/webhooks

FRY will expose IMAP for existing business integrations and offer API/webhooks, allowing existing consumers to keep working while enabling additional integration interfaces. Existing consumers report richer IMAP usage involving folders, moving messages, or server-side searches; their required search scope is email headers, dates, and flags such as read/unread. Exact folder/move operations, shared state across interfaces, delivery acknowledgments, and library selection still need investigation.

Existing integrations expect conventional email behavior and must work without enabling filtering or application routing. Teams may adopt advanced features gradually. Conventional shared mailbox state is the default: clients accessing the same mailbox see each other's flags, moves, and deletions. Independent per-application mailbox views are optional; no immediate consumer rewrite is assumed. See [optional features](0013-optional-filtering-and-routing.md).

Milestone scope: conventional shared IMAP access is in the first production milestone; application-facing API/webhook delivery and independent application views follow later. See [milestone sequencing](0014-prove-conventional-email-first.md).

For the first rollout, existing applications may switch to new FRY-issued usernames/passwords for IMAP and SMTP submission. Integration with an existing identity system and preservation of old credentials are not prerequisites. Credential scope, provisioning, rotation/revocation, and supported authentication mechanisms remain to be specified.
