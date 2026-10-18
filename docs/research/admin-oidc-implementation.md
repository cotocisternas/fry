# Administration identity and OIDC implementation

Investigated 2026-10-17. Scope: a concrete Go seam for local administrator login and generic OIDC, with authorization and session behavior stated explicitly. This is implementation research, not a production readiness or security audit. No provider registration, real credentials, or external communication was needed.

## Existing decisions and proposed boundary

[ADR 0004](../adr/0004-organization-isolation.md) and [ADR 0014](../adr/0014-prove-conventional-email-first.md) require local administrator accounts plus one shared OIDC provider per installation. Group/claim mappings automatically provision access, including explicitly configured global-operator grants. Organization managers administer only their assigned organizations and cannot delegate access. Content access is a separate permission and must be audited. Administrator identities are separate from IMAP/SMTP application credentials.

**Proposed implementation:** one `adminauth` service owns authentication, identities, server-side sessions, mapping reconciliation, and authorization. Thin HTTP handlers cover local login/logout, OIDC start/callback, and current-session information. Administrative operations receive an authenticated actor and check the target organization and permission in their transaction. A later web/API/CLI layer should call the same administrative service, rather than reproduce role checks in each interface. This is an engineering proposal; exact session durations, mapping names, password policy, provider-specific claim sources, and revocation guarantees are not settled ADR choices.

## Dependencies and exact APIs

Pin these compatible versions rather than selecting a moving branch:

| Module | Version / inspected commit | Local source |
| --- | --- | --- |
| `github.com/coreos/go-oidc/v3` | `v3.21.0`, `c914bd380327a5a3a81403774d1a5d5b73772ce7` | `/home/coto/go/pkg/mod/github.com/coreos/go-oidc/v3@v3.21.0` |
| `golang.org/x/oauth2` | `v0.36.0`, `4d954e69a88d9e1ccb8439f8d5b6cbef230c4ef9` | `/home/coto/go/pkg/mod/golang.org/x/oauth2@v0.36.0` |

Both declare Go 1.25. go-oidc v3.21.0 is the inspected latest release and depends on go-jose/v4 v4.1.4 and oauth2 v0.36.0. Latest oauth2 v0.37.0 requires Go 1.26, so adopting it would change FRY's current minimum. This recommendation retains that minimum; version recency alone does not establish absence of vulnerabilities. [go-oidc release](https://github.com/coreos/go-oidc/releases/tag/v3.21.0), [module](https://github.com/coreos/go-oidc/blob/v3.21.0/go.mod), [oauth2 v0.36.0 module](https://go.googlesource.com/oauth2/+/refs/tags/v0.36.0/go.mod), [v0.37.0 module](https://go.googlesource.com/oauth2/+/refs/tags/v0.37.0/go.mod).

The required flow is small:

```go
provider, err := oidc.NewProvider(clientCtx, configuredIssuer)
verifier := provider.VerifierContext(clientCtx, &oidc.Config{
    ClientID: clientID,
    SupportedSigningAlgs: allowedAlgorithms,
})
config := oauth2.Config{
    ClientID: clientID, ClientSecret: secret,
    Endpoint: provider.Endpoint(), RedirectURL: fixedCallback,
    Scopes: []string{oidc.ScopeOpenID}, // additional configured scopes if needed
}
pkce := oauth2.GenerateVerifier()
url := config.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(pkce))
// After validating and consuming the browser-bound login transaction:
tokens, err := config.Exchange(ctx, code, oauth2.VerifierOption(pkce))
raw, ok := tokens.Extra("id_token").(string)
identity, err := verifier.Verify(ctx, raw)
```

Handle every error and a missing ID token before proceeding. `GenerateVerifier` uses 32 random bytes; generate a fresh verifier per attempt. `oidc.ClientContext(ctx, boundedHTTPClient)` supplies the client to discovery, token exchange, and verifier key fetches when consistently propagated. These APIs do not store browser state or create application sessions. [OIDC provider API](https://github.com/coreos/go-oidc/blob/v3.21.0/oidc/oidc.go), [verifier API](https://github.com/coreos/go-oidc/blob/v3.21.0/oidc/verify.go), [PKCE implementation](https://go.googlesource.com/oauth2/+/refs/tags/v0.36.0/pkce.go), [OAuth configuration](https://go.googlesource.com/oauth2/+/refs/tags/v0.36.0/oauth2.go).

## Checks the adapter must own

Use authorization code plus S256 PKCE. Keep callback URI fixed to installation configuration; never derive it from arbitrary Host/forwarded headers or accept an unrestricted post-login redirect. Maintain independent random state, nonce, and verifier in an expiring, one-use login transaction bound to a separate browser cookie. Validate state and browser binding even for provider error callbacks, and atomically consume the transaction to reject concurrent replay. PKCE does not replace browser transaction handling in this design. [OAuth security recommendations](https://www.rfc-editor.org/rfc/rfc9700.html#section-2.1), [OIDC nonce processing](https://openid.net/specs/openid-connect-core-1_0.html#NonceNotes).

Leave all verifier skip options false. It verifies signature, configured issuer, audience membership, expiry, and not-before with built-in leeway. **It does not check nonce, authorized party (`azp`), nonempty subject, or issued-at freshness.** Decode extra claims only after Verify succeeds. Require the saved nonce, nonempty subject, required issuance/expiry claims, and configured freshness/skew policy; enforce `azp == clientID` when present and require it for multiple audiences as a conservative policy. Explicitly compare returned issuer to the configured issuer: the library has a Google-specific relaxed issuer exception. An algorithm allowlist is configuration, not a reason to enable `none`. [Verifier implementation](https://github.com/coreos/go-oidc/blob/v3.21.0/oidc/verify.go), [ID-token validation rules](https://openid.net/specs/openid-connect-core-1_0.html#IDTokenValidation).

Configure HTTPS issuer and endpoints, exact discovery issuer matching, bounded request time and response bytes, and controlled redirects. The library reads discovery bodies in full; its remote-key-set context deliberately ignores cancellation, making HTTP client timeouts material. Do not accept arbitrary issuer URLs from login requests. A private on-premises provider may be legitimate, so endpoint access restrictions must follow explicit installation configuration. Local tests may use a deliberately test-only HTTP provider. [Discovery requirements](https://openid.net/specs/openid-connect-discovery-1_0.html#ProviderConfigurationValidation), [discovery implementation](https://github.com/coreos/go-oidc/blob/v3.21.0/oidc/oidc.go), [key fetch implementation](https://github.com/coreos/go-oidc/blob/v3.21.0/oidc/jwks.go).

## Identity, mappings, and local accounts

Use a unique `(issuer, subject)` external identity key pointing to an internal administrator ID. Neither email nor preferred username identifies that account; do not auto-link a local account by matching email, even if email_verified is true. Store display fields separately. Explicit account linking can be deferred. OIDC identifies stable users through issuer and subject; other profile claims can change or be reused. [Claim stability](https://openid.net/specs/openid-connect-core-1_0.html#ClaimStability).

**Provisional mapping design:** global operators configure bounded exact-match rules over named top-level signed ID-token string or string-array claims. Each rule grants specified organization-manager assignments, a separately specified content-access permission, or an explicit global-operator role. No team/group names are implied by the product terminology. Reject malformed claim types and unsupported claim-resolution references; do not turn arbitrary claim strings into roles or organization IDs. Group claims are not standardized by OIDC. Initial ID-token-only mappings are a documented provider configuration requirement, not support for every provider's group API. If UserInfo is later supported, verify that its subject matches the ID token before using it. [Additional claims](https://openid.net/specs/openid-connect-core-1_0.html#AdditionalClaims), [UserInfo validation](https://openid.net/specs/openid-connect-core-1_0.html#UserInfoResponseValidation).

At successful login, transactionally upsert the stable identity and replace only OIDC-derived grants using the current mapping version. Record grant provenance separately from operator-managed local grants. Proposed conservative behavior: an unmatched identity receives no usable administrative session; an existing disabled identity stays disabled. Never accumulate old mapped grants indefinitely. Serialize mapping changes and login reconciliation, or check the mapping generation before commit. Local grants must not be silently removed by claim reconciliation; an account disable overrides both grant sources. Policy for intentionally mixing local grants with mapped access must be visible to operators.

For local accounts, use a persistent administrator credential record and versioned salted password hash, distinct from `internal/credentials`' immutable protocol password map. The existing standard-library PBKDF2 primitive can be reused without sharing accounts, salts, or authentication namespaces. Bound concurrent expensive verification, limit attempts, use generic failure responses, and invalidate sessions on credential reset/disable. Bootstrap the first local operator through an explicit local administrative operation, not a public first-user-wins registration endpoint. Password-hash cost, password policy, recovery, and MFA are implementation/deployment work; this note does not declare the existing prototype verifier a complete administrator account system. [Go PBKDF2 API](https://pkg.go.dev/crypto/pbkdf2).

## Sessions, reconciliation, and revocation

**Proposed first implementation:** random opaque session cookies with only a token hash in PostgreSQL. Rotate session identity after either login path. Persist account, authentication method, creation/last-use/absolute-expiry times, and relevant account/policy generation. Use Secure, HttpOnly, host-only cookies and SameSite=Lax for the normal top-level GET OIDC callback. Protect local login and administrative mutations against CSRF; state-changing actions must not use GET. Go's `http.CrossOriginProtection` is available, but OIDC callbacks still need their dedicated state checks and older-client behavior merits explicit tests. [Go HTTP cookie and origin protection APIs](https://pkg.go.dev/net/http#CrossOriginProtection).

Authorize each request from current account status, active session, current local grants, and valid OIDC-derived grants. Database errors fail closed. Checking only roles embedded in an old cookie or JWT would defeat local revocation. For sensitive mutations, keep authorization and update within an appropriate transaction so revocation cannot race a stale check. A request already executing when access is revoked needs explicit semantics; do not claim all in-flight operations are instantly canceled.

Local logout revokes the FRY session. Operator disabling an account or revoking sessions affects subsequent checked requests. Proposed mapping-edit behavior is to invalidate affected OIDC sessions and require new login rather than recompute privileges from old cached claims. Provider-side group removal or account disable is **not automatically pushed by ordinary OIDC login**: login-time reconciliation learns changes on reauthentication. Configure bounded session and claim-freshness lifetimes and require reauthentication at their boundary. Do not retain access indefinitely on provider outage. No numeric production revocation SLA is established here.

Skip refresh-token/offline access in the first seam unless needed. Refreshing an access token alone does not prove that group membership was re-evaluated. Back-channel logout is a possible later capability requiring signed logout-token validation and session correlation; it does not by itself guarantee notifications for every group change. Local FRY logout is distinct from provider-wide logout. [OIDC refresh behavior](https://openid.net/specs/openid-connect-core-1_0.html#RefreshTokens), [back-channel logout](https://openid.net/specs/openid-connect-backchannel-1_0.html).

## Test strategy and unresolved deployment choices

Use a local `httptest` provider with discovery, authorization, token, and JWKS endpoints and real asymmetric signed ID tokens. Assert actual authorization requests contain state, nonce, S256 challenge, and the fixed redirect URI. Token exchange must verify one-use code, matching verifier, and client authentication. Exercise the HTTP handlers with a cookie jar and redirects, and persist identity/session/grant behavior against disposable PostgreSQL.

Tests should cover wrong issuer/audience/azp/signature/algorithm/nonce, missing subject/issued-at, expiry and skew, invalid browser state/binding, callback replay and concurrent callbacks, key rotation, discovery/JWKS failure and response limits, duplicate identity provisioning, same email with distinct subjects/issuers, malformed/absent groups, explicit global mapping, organization isolation, independent content access, nondelegating managers, grant replacement versus local provenance, disabled accounts, revoked/expired sessions, mapping changes during login, local password reset, CSRF, and audit access. Use fixture-only claims and organization IDs without implying actual team names.

Before public deployment, choose the real issuer/client registration, trusted endpoints and proxy origin, exact claim sources/mappings, signing policy, login/session/freshness/skew values, password/recovery/MFA policy, bootstrap procedure, audit retention, and operational revocation target. These are explicit provisional settings or deployment gates. They do not prevent implementing and validating the seam now.
