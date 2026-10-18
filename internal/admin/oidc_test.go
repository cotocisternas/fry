//go:build linux

package admin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func policyConfig(org string) OIDCPolicyConfig {
	return OIDCPolicyConfig{
		Issuer:            "https://issuer.example.test",
		ClientID:          "fry-client",
		SigningAlgorithms: []string{"RS256"},
		LoginLifetime:     5 * time.Minute,
		TokenMaxAge:       5 * time.Minute,
		ClockSkew:         time.Second,
		Rules: []ClaimRule{
			{Claim: "groups", Value: "managers", Grants: []Grant{{Scope: org, Content: true}}},
		},
	}
}

func policyLogin(t *testing.T, policy *OIDCPolicy) OIDCLogin {
	t.Helper()
	state, err := opaque()
	check(t, err)
	input := OIDCLoginStart{
		State:          state,
		BrowserBinding: strings.Repeat("b", 43),
		Nonce:          strings.Repeat("n", 43),
		Verifier:       strings.Repeat("v", 43),
	}
	check(t, policy.CreateLogin(t.Context(), input))
	{
		_, err := policy.ConsumeLogin(t.Context(), state, strings.Repeat("x", 43))
		requireErrorIs(t, err, ErrUnauthorized)
	}
	login, err := policy.ConsumeLogin(t.Context(), state, input.BrowserBinding)
	check(t, err)
	if login.Nonce != input.Nonce || login.Verifier != input.Verifier {
		t.Fatal("browser verification material changed")
	}
	{
		_, err := policy.ConsumeLogin(t.Context(), state, input.BrowserBinding)
		requireErrorIs(t, err, ErrUnauthorized)
	}
	return login
}

func TestOIDCPolicyGenerationAndFrozenMapping(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	cfg := policyConfig(f.org)
	policy, err := f.service.ConfigureOIDC(ctx, cfg)
	check(t, err)
	// The application's grant policy must not follow later caller-owned edits.
	cfg.Rules[0].Grants[0] = Grant{Scope: "*", Content: true}
	identity := VerifiedOIDCIdentity{
		Subject: "policy-subject",
		Claims:  map[string]json.RawMessage{"groups": json.RawMessage(`["managers"]`)},
	}
	session, err := policy.CompleteLogin(ctx, policyLogin(t, policy), identity)
	check(t, err)
	if len(session.Identity.Grants) != 1 || session.Identity.Grants[0].Scope != f.org ||
		!session.Identity.Grants[0].Content ||
		session.Identity.Grants[0].Source != "oidc" {
		t.Fatalf("mapping escaped frozen policy: %+v", session.Identity.Grants)
	}
	pending := policyLogin(t, policy)
	replacement, err := f.service.ConfigureOIDC(ctx, cfg)
	check(t, err)
	{
		_, err := f.service.Identity(ctx, session.Token)
		requireErrorIs(t, err, ErrUnauthorized)
	}
	{
		_, err := policy.CompleteLogin(ctx, pending, identity)
		requireErrorIs(t, err, ErrUnauthorized)
	}
	{
		_, err := replacement.CompleteLogin(ctx, pending, identity)
		requireErrorIs(t, err, ErrUnauthorized)
	}
	input := OIDCLoginStart{
		State:          strings.Repeat("s", 43),
		BrowserBinding: strings.Repeat("b", 43),
		Nonce:          strings.Repeat("n", 43),
		Verifier:       strings.Repeat("v", 43),
	}
	requireErrorIs(t, policy.CreateLogin(ctx, input), ErrUnauthorized)
	check(t, f.service.DisableOIDC(ctx))
	requireErrorIs(t, replacement.CreateLogin(ctx, input), ErrUnauthorized)
}

func TestOIDCProvisioningRollsBackWhenAuditFails(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	policy, err := f.service.ConfigureOIDC(ctx, policyConfig(f.org))
	check(t, err)
	identity := VerifiedOIDCIdentity{
		Subject: "atomic-subject",
		Claims:  map[string]json.RawMessage{"groups": json.RawMessage(`["managers"]`)},
	}
	session, err := policy.CompleteLogin(ctx, policyLogin(t, policy), identity)
	check(t, err)
	// A real audit failure must roll back changed claim grants and session deletion.
	_, err = f.db.Exec(
		ctx,
		`CREATE FUNCTION reject_oidc_audit() RETURNS trigger LANGUAGE plpgsql AS $$ `+
			`BEGIN IF NEW.action='oidc-grants-reconcile' THEN RAISE EXCEPTION 'synthetic `+
			`audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_oidc_audit `+
			`BEFORE INSERT ON fry_admin_audit FOR EACH ROW EXECUTE FUNCTION `+
			`reject_oidc_audit()`,
	)
	check(t, err)
	identity.Claims["groups"] = json.RawMessage(`[]`)
	if result, err := policy.CompleteLogin(ctx, policyLogin(t, policy), identity); err == nil || result.Token != "" {
		t.Fatal("audit failure published provisioning")
	}
	who, err := f.service.Identity(ctx, session.Token)
	check(t, err)
	if len(who.Grants) != 1 || who.Grants[0].Scope != f.org {
		t.Fatal("failed reconcile removed old grants")
	}
	_, err = f.db.Exec(ctx, `DROP TRIGGER reject_oidc_audit ON fry_admin_audit`)
	check(t, err)
	{
		_, err := policy.CompleteLogin(ctx, policyLogin(t, policy), identity)
		requireErrorIs(t, err, ErrForbidden)
	}
	{
		_, err := f.service.Identity(ctx, session.Token)
		requireErrorIs(t, err, ErrUnauthorized)
	}
	// Removal and its audit commit together even though login is denied.
	events, err := f.service.Audits(context.Background(), f.root.Token, 0)
	check(t, err)
	reconciled := 0
	for _, event := range events {
		if event.Action == "oidc-grants-reconcile" && event.Actor == session.Identity.ID {
			reconciled++
		}
	}
	if reconciled != 2 {
		t.Fatalf("committed reconciliation audits = %d, want 2", reconciled)
	}
}

func TestOIDCClaimMappingRejectsMalformedTrailingValues(t *testing.T) {
	policy := &OIDCPolicy{config: policyConfig(strings.Repeat("a", 32))}
	excessive, err := json.Marshal(make([]string, 1025))
	check(t, err)
	cases := []json.RawMessage{
		json.RawMessage(`null`),
		json.RawMessage(`17`),
		json.RawMessage(`{"managers":true}`),
		json.RawMessage(`["managers",17]`),
		json.RawMessage(`["managers","` + strings.Repeat("x", 4097) + `"]`),
		json.RawMessage(strings.Repeat(" ", 64<<10) + `"managers"`),
		excessive,
	}
	for _, raw := range cases {
		_, err := policy.mapClaims(map[string]json.RawMessage{"groups": raw})
		requireErrorIs(t, err, ErrUnauthorized)
	}
}

func TestOIDCClaimMappingUnionsContentAndSortsScopes(t *testing.T) {
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	policy := &OIDCPolicy{config: OIDCPolicyConfig{Rules: []ClaimRule{
		{Claim: "groups", Value: "managers", Grants: []Grant{{Scope: b}, {Scope: a}}},
		{Claim: "groups", Value: "readers", Grants: []Grant{{Scope: a, Content: true}}},
	}}}
	grants, err := policy.mapClaims(map[string]json.RawMessage{"groups": json.RawMessage(`["managers","readers"]`)})
	check(t, err)
	if !reflect.DeepEqual(grants, []Grant{{Scope: a, Content: true, Source: "oidc"}, {Scope: b, Source: "oidc"}}) {
		t.Fatal("claim grants lost union, order or provenance", grants)
	}
	grants, err = policy.mapClaims(map[string]json.RawMessage{"groups": json.RawMessage(`"managers"`)})
	check(t, err)
	if len(grants) != 2 || grants[0].Content {
		t.Fatal("scalar claim changed content privilege", grants)
	}
	grants, err = policy.mapClaims(nil)
	check(t, err)
	if len(grants) != 0 {
		t.Fatal("absent claims granted access", grants)
	}
}

func TestOIDCPolicyValidationRejectsInvalidConfigurationBeforeDatabase(t *testing.T) {
	cases := map[string]func(*OIDCPolicyConfig){
		"issuer":         func(c *OIDCPolicyConfig) { c.Issuer = "" },
		"client":         func(c *OIDCPolicyConfig) { c.ClientID = "" },
		"client size":    func(c *OIDCPolicyConfig) { c.ClientID = strings.Repeat("x", 1025) },
		"login short":    func(c *OIDCPolicyConfig) { c.LoginLifetime = time.Second },
		"login long":     func(c *OIDCPolicyConfig) { c.LoginLifetime = time.Hour },
		"token short":    func(c *OIDCPolicyConfig) { c.TokenMaxAge = time.Second },
		"token long":     func(c *OIDCPolicyConfig) { c.TokenMaxAge = time.Hour },
		"negative skew":  func(c *OIDCPolicyConfig) { c.ClockSkew = -1 },
		"excessive skew": func(c *OIDCPolicyConfig) { c.ClockSkew = time.Hour },
		"algorithms":     func(c *OIDCPolicyConfig) { c.SigningAlgorithms = nil },
		"rule count":     func(c *OIDCPolicyConfig) { c.Rules = make([]ClaimRule, 257) },
		"claim":          func(c *OIDCPolicyConfig) { c.Rules[0].Claim = "" },
		"claim size":     func(c *OIDCPolicyConfig) { c.Rules[0].Claim = strings.Repeat("x", 129) },
		"value":          func(c *OIDCPolicyConfig) { c.Rules[0].Value = "" },
		"value size":     func(c *OIDCPolicyConfig) { c.Rules[0].Value = strings.Repeat("x", 1025) },
		"grants":         func(c *OIDCPolicyConfig) { c.Rules[0].Grants = nil },
		"grant count":    func(c *OIDCPolicyConfig) { c.Rules[0].Grants = make([]Grant, 257) },
		"grant scope":    func(c *OIDCPolicyConfig) { c.Rules[0].Grants[0].Scope = "invalid" },
	}
	service := &Service{}
	for name, alter := range cases {
		t.Run(name, func(t *testing.T) {
			config := policyConfig(strings.Repeat("a", 32))
			alter(&config)
			_, err := service.ConfigureOIDC(t.Context(), config)
			requireErrorIs(t, err, ErrInvalid)
		})
	}
}
