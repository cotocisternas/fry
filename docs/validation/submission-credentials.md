# Live SMTP submission credentials

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

SMTP credentials can be created and managed without restarting listeners through
the web interface, administrative HTTP API and `fry-admin`. PostgreSQL is
authoritative for each credential's organization, permitted sender addresses and
domains, password hash, enabled state and generation. This is send-only access;
assigned organization managers need configuration permission, not message-content
permission. Global operators can manage every organization. Managers can operate
across their explicit assignments but cannot delegate privileges or escape those
assignments through a request's organization field.

The live creation/policy implementation, focused tests, integrated process
rehearsal and local Chromium browser verification passed. The full PostgreSQL
race suite and `go vet ./...` also passed after integration. Earlier rotate/disable
browser evidence below is explicitly historical.

## Contract and authority

Creation is create-only: a globally unique SMTP username cannot silently reset
an existing credential or move it to another organization. Duplicate names
return a generic conflict. Existing password rotation, enabled-state changes and
policy updates are separate operations. There is no hard-delete or organization-
rebind operation. At least one permitted address or domain is required; the
backend validates and normalizes bounded arrays (128 combined entries and 16 KiB
of strings). Domains are lowercase; address local parts retain case.

Live SMTP authentication resolves persisted credentials rather than a startup
username allowlist. Credential-generation checks fence preparation and durable
finalization, including previously authenticated sessions. Password, access or
policy changes invalidate stale proofs; an empty or narrower current sender
policy cannot be bypassed by an older AUTH session. Public submission additionally
requires organization-scoped outgoing sender authority: credential scope and
sender grants intersect. Creating an incoming domain or SMTP credential does
not authorize an outgoing identity. Default/organization relay selection remains
trusted deployment configuration and is not editable through credential APIs.

Both receipt preparation and finalization check the authenticated generation
under a shared account-row lock. Revocation that commits first blocks new
acceptance; acceptance holding the lock first completes before the change.
Durable status resolves ambiguous acceptance replies. Previously accepted work
retains its queue and selected relay; disabling a credential does not withdraw
accepted mail. Clients rejected for stale credentials reconnect and authenticate
again with current credentials.

Startup configuration is an insert-only bootstrap. Existing rows retain their
organization, sender policy, password, enabled state and generation despite
stale or absent bootstrap secrets or scopes. Removing a bootstrap entry does
**not** revoke the persisted credential; use disable or policy editing. An empty
bootstrap list supports accounts created live. Required policy schema is checked
even with an empty list; an older schema fails closed. `-init` creates fresh
schemas and is not an upgrade/migration mechanism.

Successful mutations commit actor, organization, username and action audit with
the credential change. Passwords and hashes are omitted from listings, mutation
responses, CLI output and audits. Hashing is bounded; authorization is rechecked
transactionally after hashing. HTTP mutations retain exact Origin and session
CSRF requirements. The UI clears creation/rotation secrets before awaiting a
request and on organization changes/logout; delayed responses cannot replace
another organization's view. Policy editing sends no password or enabled flag.

## API and CLI

The existing authenticated administration session and CSRF rules apply:

| Operation | Endpoint / fields |
| --- | --- |
| List | `GET /api/submission-accounts?organization=…` |
| Create only | `POST /api/submission-accounts`: `organization`, `username`, `password`, `authorized_senders`, `authorized_domains`, explicit `enabled` |
| Change permitted senders | `POST /api/submission-policy`: `organization`, `username`, `authorized_senders`, `authorized_domains` |
| Rotate password | `POST /api/submission-password`: `organization`, `username`, `password` |
| Enable/disable | `POST /api/submission-access`: `organization`, `username`, `enabled` |

Both sender fields are JSON arrays. Lists expose scopes and account metadata,
never password material. Invalid inputs, authorization failures, missing targets,
name conflicts and authority outages have distinct generic error responses.

For a provisioned disposable administration endpoint, supply
`FRY_ADMIN_PASSWORD` and `FRY_NEW_SUBMISSION_PASSWORD` through your environment,
then run:

```sh
fry-admin -organization "$ORG_ID" -name automation \
  -authorized-senders billing@example.test \
  -authorized-domains example.test -enabled=true submission-account-create
fry-admin -organization "$ORG_ID" -name automation \
  -authorized-domains example.test submission-policy-set
fry-admin -organization "$ORG_ID" submission-accounts
```

Add the configured `-origin` when not using the default numeric-loopback origin.
Sender flags accept comma-separated lists; backend validation remains authoritative.
Creation/rotation reads `FRY_NEW_SUBMISSION_PASSWORD`, never a password argument.
Policy editing neither requires nor transmits that secret. Password rotation
preserves disabled state. Existing `submission-password-set` and
`-enabled=false submission-access-set` operations remain available (all flags
precede the command).

## Validation evidence and limits

Focused CLI tests execute the real command path against a synthetic HTTP server,
checking endpoint, CSRF, scope arrays, explicit disabled creation, environment-only
password transport and absence of secrets from output. Node-based UI behavior
tests check password clearing before requests, policy payload separation, and
stale success/error suppression across organization/session changes. JavaScript
syntax and focused race tests passed; source review found no actionable P1/P2.
Backend component tests cover creation conflict/uniqueness, scoped administration,
live policy/credential generations, insert-only bootstrap, audit and protocol
fencing. Current browser evidence is recorded below; no public deployment or
production-capacity claim follows.

The previous dedicated process smoke, before live provisioning, passed on
2026-10-17 using actual `fry`/`fry-admin`, disposable PostgreSQL18 pinned to
`postgres@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336`,
and two private local member roots. It validated three exact queued originals,
two clean restarts, old/new password behavior, stale AUTH rejection at MAIL and
final DATA, wrong-organization/missing-account rejection, and preservation of
password/disabled state across stale then absent bootstrap secrets. Already
accepted work handed off unchanged after disable. Its transport was permitted
loopback plaintext; it did not validate new account creation or policy editing.
The prior combined PostgreSQL race suite and `go vet ./...` passed after that
stage, as did separate TLS DATA/BDAT credential-revocation tests.

A historical local Chromium walkthrough verified listing, rotation, disable,
rotation while disabled, re-enable and password/form clearing on organization
switch, without page errors. The shared browser timed out and the checks used a
local Playwright runner; the fixture was cleaned. This walkthrough also predates
live creation/policy UI and is separate from the current live-provisioning browser validation below.

The process smoke remains reproducible with:

```sh
scripts/smoke.sh submission-credentials
```

It uses only disposable synthetic resources and bounded local relay/process/network
operations, no external mail or cloud resources. The expanded process rehearsal passed with exit 0; its report is recorded below.


## Current integrated process result

On 2026-10-17, the expanded smoke completed with seven exact original handoffs,
four clean restarts, and checked fixture cleanup. Evidence log:
`/tmp/fry-live-submission-smoke-final.log` (temporary local artifact; final
success is printed only after temporary fixture cleanup). It required
verified STARTTLS for real SMTP authentication and proved:

- Account creation and sender-policy changes became usable without restart.
- Policy revocation fenced stale MAIL and final DATA; accepted queued work still
  handed off exactly, without duplicate relay deliveries.
- Disabled usernames remained reserved and could not be recreated to replace
  an existing account.
- Stale and absent bootstrap secrets/policy did not overwrite persistent state;
  empty bootstrap retained created accounts after restart.
- A newly administered organization used the configured default relay while
  cross-organization sender scope was rejected.
- Wrong-organization/missing-account CLI operations and stale authenticated
  sessions were rejected, and persisted sender restrictions remained enforced.

All emitted success flags were true, including `verified_starttls_required`,
`live_account_and_policy_without_restart`,
`policy_revocation_fenced_mail_and_data`, `disabled_username_reserved`, and
`empty_bootstrap_and_new_org_default_relay`. This is local synthetic process,
protocol and restart evidence, not a public deployment, load, crash takeover or
availability guarantee. The full PostgreSQL race suite passed (exit 0,
`/tmp/fry-live-submission-race.log`) and `go vet ./...` passed.


## Current browser verification

A fresh disposable demo on numeric-loopback port 8083 passed local Playwright
with system Chromium using `/tmp/fry-live-submission-browser.cjs`. The native
preview opened/navigated, but two snapshot calls each failed with
`Preview automation snapshot timed out after 15000ms.`; the authorized local
fallback then completed the checks. The browser created a credential in a new
empty organization, verified normalized returned sender scopes, edited policy
without transmitting password/enabled fields, and checked duplicate creation
conflicted without overwriting the prior row. Disabling then editing policy
preserved disabled state.

The browser also verified that passwords cleared before a held request response,
on a synthetic 503 failure, organization switch and logout. It held a successful
creation response, switched organizations, then released it: the current empty
organization remained visible and stale completion did not replace its view.
There were zero page JavaScript errors. These checks used a global operator;
manager multi-assignment/non-content permissions remain backend integration-test
evidence rather than a separate browser assertion. The browser, demo process,
temporary files and disposable database were removed after verification; the
other existing demo instance was left untouched.
No public exposure, performance or production-readiness claim follows.
