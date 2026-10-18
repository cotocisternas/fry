# Local administration and directory evidence

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Implemented and exercised on 2026-10-17. This is a localhost prototype against disposable PostgreSQL and synthetic mail. It does not establish public-edge security, production identity-provider compatibility, distributed authorization availability, or AWS recovery guarantees.

## Implemented behavior

`internal/admin` owns persistent local administrator identities, separate from mail-protocol credentials. Passwords have versioned salted PBKDF2-SHA256 encodings with 600,000 iterations. Verification concurrency is bounded; plaintext passwords are not stored in the database. Explicit bootstrap creates the first global operator only when there are no administrator identities. Repeated startup cannot reset that account. Operator-managed account changes, role assignments, and content access are audited.

Sessions use random opaque cookies with only their hashes stored in PostgreSQL. Each request checks the current account status, account generation, expiry, grants, and applicable OIDC policy generation. Password reset, account disablement, and session logout invalidate existing authorization. Only global operators may manage administrator accounts or replace local grants. Organization managers see and configure only assigned organizations. At least one enabled global operator must remain after an explicit local grant/account mutation; this is not a full emergency-access/recovery system.

Configuration permission does not authorize complete email downloads. An explicit content grant is required for the requested organization (or globally), and a committed audit entry precedes opening the raw-content reader. If audit persistence fails, no reader is returned. Downloads use attachment disposition and stream original bytes without copying messages into SQL. Closing records server-side read completion or interruption; it cannot prove a browser saved the message. A missing completion update leaves the original `authorized` audit entry. Revocation blocks subsequent checked requests; an already authorized transfer is allowed to finish.

Issuing, resetting, or re-enabling a mailbox credential also requires content permission: a usable mailbox password is itself a content-access capability. Configuration-only administrators can disable an existing mailbox credential. Regular application IMAP reads do not create administrator content-download audits; credential changes are audited separately.

`internal/maildirectory` holds incoming domain ownership, explicit aliases, optional catch-all inboxes, and persistent IMAP credentials. A domain belongs to exactly one organization. Alias destinations must belong to that organization; repeated destinations collapse. Disabled explicit aliases do not fall through to a catch-all. Unknown recipients are rejected; a directory/database outage yields a temporary SMTP failure. Routing is resolved at RCPT time and the accepted destination snapshot remains stable for that transaction.

The executable seeds missing directory records from startup JSON. Persistent administrator edits, disabled routes, and rotated passwords survive restart and override old bootstrap values. Live SMTP reads the directory without restart. IMAP validates the credential generation on each operation that accesses mailbox state/content; disabling or resetting a credential prevents further checked operations on existing sessions. In-flight work retains its initial authorization. Static account/route constructors remain available for isolated protocol fixtures. [SMTP credentials](submission-credentials.md) can now be created live and have persisted sender restrictions, passwords, enabled state and generations. Assigned organization managers need configuration permission, not message-content permission, for these send-only operations. Bootstrap entries insert missing accounts only; removing them does not revoke persisted credentials. Relay selection remains trusted deployment configuration, and public sender grants intersect credential restrictions.

The web interface, administrative HTTP endpoints, and `cmd/fry-admin` cover organizations, inboxes, incoming domains, aliases, IMAP and SMTP credentials, administrator accounts/grants, receipt metadata, audited raw downloads, and audit events. Metadata/audit results are bounded; receipts and audit events support cursors. Organization, inbox, domain, address and account lists currently stop at 1,000 entries. The interface labels the installation as local development. There is no rendered email HTML preview.

HTTP requests require the configured Host. Mutations require the exact configured Origin, plus a per-session CSRF value except for the login endpoint (which still requires Origin). Cookies are HttpOnly and SameSite=Lax; HTTPS origins use Secure host-prefixed cookies. Plain HTTP origins are accepted only on numeric loopback. The runnable server binds to loopback, limits body/header sizes and request concurrency, and supplies no-store, nosniff, CSP, and referrer headers. This is not permission to expose the prototype directly to the internet.

## OIDC

The adapter follows the [implementation research](../research/admin-oidc-implementation.md), using pinned coreos/go-oidc v3.21.0 and oauth2 v0.36.0. It supports one installation provider, authorization code + S256 PKCE, browser-bound one-use state, a nonce, and real signature verification. Issuer, audience, expiry, authorized party, subject, nonce, not-before against configured clock skew, and issued-at freshness are checked. Issuer/subject identifies the account; matching email claims never link identities. HTTPS endpoints are required except explicitly enabled numeric-loopback test providers. Discovery/token/JWKS responses and network time are bounded; endpoint redirects are refused.

Rules match exact values in signed ID-token top-level string or string-array claims. They grant explicit organization or global access, with content permission separately specified. This does not fetch provider-specific group APIs or UserInfo, refresh tokens, or provide back-channel logout. Actual issuer/client registration, claim rules, and signing policy remain deployment configuration.

Successful reauthentication replaces OIDC-derived grants while preserving local grants. Removing every usable grant commits the removal and revokes old sessions even though the new login is denied. A disabled identity cannot re-enable itself by signing in. Changing configured mappings/provider increments a policy generation, removes old mapped grants, and invalidates OIDC sessions and pending login transactions. Removing OIDC configuration also invalidates those sessions. Multiple active processes must use the same configuration generation; this prototype does not implement distributed configuration rollout.

Ordinary OIDC login does not push later provider-side group changes to FRY. Local revocation is checked on subsequent requests; provider changes are learned on reauthentication, bounded by the explicitly configured absolute session lifetime. No production revocation SLA is implied. The example's 60-minute local session lifetime is a disposable demonstration value.

## Reproduce

```sh
./scripts/tests/receipts.sh -race -short
./scripts/smoke.sh smtp
make demo
```

The demo creates disposable PostgreSQL, two local byte members, and synthetic mail. Visit `http://127.0.0.1:8080`; the fixture-only login is printed by the script. Ctrl-C removes the process, database, and local data. Set `FRY_DEMO_HTTP_PORT` if needed. For an independently managed local fixture, use `examples/local-admin.json` with the documented database, IMAP/submission, and `FRY_LOCAL_ADMIN_PASSWORD` environment variables. `-init` applies fresh receipt and administration schemas; neither is an upgrade mechanism.

Example CLI against the disposable demo:

```sh
FRY_ADMIN_PASSWORD=disposable-demo-admin go run ./cmd/fry-admin organizations
FRY_ADMIN_PASSWORD=disposable-demo-admin go run ./cmd/fry-admin \
  -organization 20000000000000000000000000000001 domains
```

The CLI uses the same local login, session, CSRF checks, and authorization as the browser, and logs out on completion. Passwords come from environment variables rather than arguments. `user-save` uses `FRY_NEW_ADMIN_PASSWORD`; `mail-account-save` uses `FRY_NEW_MAIL_PASSWORD`. `download` requires a new `-out` path, creates it with mode 0600, refuses overwrite, and removes its own incomplete file on failure. OIDC browser login is not a CLI login flow yet.

Integration evidence covers account persistence, bootstrap non-reset, organization isolation, nondelegating managers, password reset/logout/grant revocation, explicit content permission, committed audit-before-read, audit outage, origin/CSRF rejection, exact streamed downloads, signed OIDC/PKCE exchange, wrong claims, callback replay/browser binding, independent issuer/subject identities, grant reconciliation/provenance, account disablement, and provider removal. Live SMTP/IMAP tests create domains and credentials after listeners start, deliver a message, disable an authenticated mailbox account, and check temporary failure during a catalog outage. A local browser walkthrough exercised login, message listing, and creation of a domain and address; the CLI listed organizations from that running process.

## Remaining production work

- Public TLS/proxy integration, distributed login throttling, MFA/recovery policy, complete login-security auditing, and real provider/browser compatibility validation.
- Administrative pagination beyond the bounded lists, detailed audit change payloads, audit retention/export/tamper resistance, and user-facing operational error workflows.
- Persistent outbound credentials/relay versions and sender verification or audited global-operator override. Creating an incoming domain does not authorize an outgoing identity.
- Recovery restoration workflows, migration orchestration, configurable organization retention/quotas/alerts, and optional filtering/routing/review interfaces.
- Schema upgrades, networked storage replication, metadata HA, throughput/fairness, and actual multi-AZ failure validation.

The browser regression `scripts/browser/admin.cjs` runs against the disposable demo (default test origin `http://127.0.0.1:8081`) with locally installed Playwright/Chromium. It holds back an organization's inbox response, switches organizations, then releases the old response and verifies that the selected organization's rows remain visible. It also creates/disables a synthetic IMAP credential and checks logout clears the prior listing. Form controls stay disabled while a view loads; navigation generations discard late responses, and organization changes reset scoped form inputs. This run used a local headless browser after the shared preview timed out twice; the earlier basic UI checks used the shared preview.


The global-only [member-storage view](member-storage-diagnostics.md#web-administration-evidence--2026-10-18)
adds read-only deployment diagnostics independently of organization selection.
Its browser regression covers exact counters, bounded pagination, lookup retry,
unknown runtime state and session isolation. Logout clears sensitive results
immediately and gates sign-in until its response completes, so an old cookie
clear cannot erase a newer session. Late HTTP 401 responses are tied to the
identity that initiated their request.
