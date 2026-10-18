//go:build linux

package admintransport

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/admin"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

type (
	authorization   struct{ nonce, challenge, redirect string }
	providerFixture struct {
		server       *httptest.Server
		key          *rsa.PrivateKey
		mu           sync.Mutex
		codes        map[string]authorization
		subject      string
		groups       any
		alter        func(map[string]any)
		lastCallback string
	}
)

func newProvider(t *testing.T) *providerFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	check(t, err)
	p := &providerFixture{
		key:     key,
		codes:   map[string]authorization{},
		subject: "subject-one",
		groups:  []string{"managers"},
	}
	p.server = httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.serveHTTP(t, w, r) }),
	)
	t.Cleanup(p.server.Close)
	return p
}

func configure(t *testing.T, h *HTTP, p *providerFixture, org string) OIDCConfig {
	t.Helper()
	cfg := OIDCConfig{
		Issuer:            p.server.URL,
		ClientID:          "fry-client",
		ClientSecret:      "fixture-client-secret",
		SigningAlgorithms: []string{"RS256"},
		LoopbackHTTP:      true,
		LoginLifetime:     5 * time.Minute,
		TokenMaxAge:       5 * time.Minute,
		ClockSkew:         time.Second,
		Rules: []admin.ClaimRule{
			{Claim: "groups", Value: "managers", Grants: []admin.Grant{{Scope: org, Content: true}}},
			{Claim: "groups", Value: "operators", Grants: []admin.Grant{{Scope: "*"}}},
		},
	}
	check(t, h.ConfigureOIDC(context.Background(), cfg))
	return cfg
}

func oidcLogin(t *testing.T, client *http.Client, origin string) int {
	t.Helper()
	response, _ := request(t, client, origin, "GET", "/auth/oidc", "", nil)
	return response.StatusCode
}

func httpIdentity(t *testing.T, client *http.Client, origin string) admin.Identity {
	t.Helper()
	response, raw := request(t, client, origin, "GET", "/api/session", "", nil)
	requireHTTPStatus(t, response, 200)
	var who admin.Identity
	check(t, json.Unmarshal(raw, &who))
	return who
}

func TestOIDCRealSignedFlowProvisioningAndRevocation(t *testing.T) {
	f := setup(t)
	h, server, client := httpFixture(t, f)
	p := newProvider(t)
	cfg := configure(t, h, p, f.org)
	if status := oidcLogin(t, client, server.URL); status != 200 {
		t.Fatalf("login: %d", status)
	}
	first := httpIdentity(t, client, server.URL)
	if first.Name != "subject-one" || len(first.Grants) != 1 || first.Grants[0].Scope != f.org {
		t.Fatalf("claims not mapped: %+v", first)
	}
	p.mu.Lock()
	callback := p.lastCallback
	p.mu.Unlock()
	response, _ := request(t, client, callback, "GET", "", "", nil)
	requireHTTPStatus(t, response, 401)
	// Identity is issuer+subject even when the provider reuses the email claim.
	p.mu.Lock()
	p.subject = "subject-two"
	p.groups = []string{"operators"}
	p.mu.Unlock()
	if status := oidcLogin(t, client, server.URL); status != 200 {
		t.Fatal(status)
	}
	second := httpIdentity(t, client, server.URL)
	if second.ID == first.ID || len(second.Grants) != 1 || second.Grants[0].Scope != "*" || second.Grants[0].Content {
		t.Fatal("email linked accounts or implicit content privilege")
	}
	checkOIDCGrantRevocation(t, f, h, p, cfg, client, server.URL, first)
}

func TestOIDCRejectsInvalidSignedClaimsAndBrowserBinding(t *testing.T) {
	f := setup(t)
	h, server, client := httpFixture(t, f)
	p := newProvider(t)
	configure(t, h, p, f.org)
	tests := map[string]func(map[string]any){
		"nonce":                      func(c map[string]any) { c["nonce"] = "wrong" },
		"issuer":                     func(c map[string]any) { c["iss"] = "https://wrong.test" },
		"audience":                   func(c map[string]any) { c["aud"] = "wrong" },
		"azp":                        func(c map[string]any) { c["azp"] = "wrong" },
		"multi-audience-without-azp": func(c map[string]any) { c["aud"] = []string{"fry-client", "other"} },
		"subject":                    func(c map[string]any) { c["sub"] = "" },
		"issued-at":                  func(c map[string]any) { delete(c, "iat") },
		"future-issued-at":           func(c map[string]any) { c["iat"] = time.Now().Add(time.Hour).Unix() },
		"stale-issued-at":            func(c map[string]any) { c["iat"] = time.Now().Add(-time.Hour).Unix() },
		"expiry":                     func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
		"malformed-groups":           func(c map[string]any) { c["groups"] = map[string]any{"operators": true} },
	}
	tests["not-before-outside-configured-skew"] = func(c map[string]any) {
		c["nbf"] = time.Now().Add(2 * time.Minute).Unix()
	}
	tests["null-not-before"] = func(c map[string]any) { c["nbf"] = nil }
	tests["string-not-before"] = func(c map[string]any) { c["nbf"] = "0" }
	for name, alter := range tests {
		t.Run(name, func(t *testing.T) {
			p.mu.Lock()
			p.alter = alter
			p.mu.Unlock()
			if status := oidcLogin(t, client, server.URL); status != 401 {
				t.Fatalf("invalid identity accepted: %d", status)
			}
		})
	}
	p.mu.Lock()
	p.alter = nil
	p.mu.Unlock()
	checkOIDCBrowserBinding(t, f, h, p, client, server.URL)
}

func (p *providerFixture) serveHTTP(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		p.discovery(t, w)
	case "/keys":
		p.keys(t, w)
	case "/authorize":
		p.authorize(t, w, r)
	case "/token":
		p.token(t, w, r)
	default:
		http.NotFound(w, r)
	}
}

func (p *providerFixture) discovery(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	if err := json.NewEncoder(w).
		Encode(map[string]any{
			"issuer":                                p.server.URL,
			"authorization_endpoint":                p.server.URL + "/authorize",
			"token_endpoint":                        p.server.URL + "/token",
			"jwks_uri":                              p.server.URL + "/keys",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}); err != nil {
		t.Error(err)
	}
}

func (p *providerFixture) keys(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	if err := json.NewEncoder(w).
		Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &p.key.PublicKey, KeyID: "one", Algorithm: "RS256", Use: "sig"},
		}}); err != nil {
		t.Error(err)
	}
}

func (p *providerFixture) authorize(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	q := r.URL.Query()
	if q.Get("client_id") != "fry-client" || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" ||
		len(q.Get("code_challenge")) != 43 ||
		q.Get("nonce") == "" ||
		q.Get("state") == "" {
		http.Error(w, "invalid authorization", 400)
		return
	}
	code, e := opaque()
	if e != nil {
		http.Error(w, "entropy", 500)
		return
	}
	p.codes[code] = authorization{q.Get("nonce"), q.Get("code_challenge"), q.Get("redirect_uri")}
	p.lastCallback = q.Get("redirect_uri") + "?code=" + code + "&state=" + q.Get("state")
	http.Redirect(w, r, p.lastCallback, http.StatusFound)
}

func (p *providerFixture) token(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if err := r.ParseForm(); err != nil {
		t.Error(err)
		http.Error(w, "form", http.StatusBadRequest)
		return
	}
	a, ok := p.consumeTokenRequest(r)
	if !ok {
		http.Error(w, "invalid token request", 400)
		return
	}
	claims := map[string]any{
		"iss":    p.server.URL,
		"aud":    "fry-client",
		"sub":    p.subject,
		"iat":    time.Now().Unix(),
		"exp":    time.Now().Add(time.Hour).Unix(),
		"nonce":  a.nonce,
		"groups": p.groups,
		"email":  "same-email@example.test",
	}
	if p.alter != nil {
		p.alter(claims)
	}
	signer, e := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: p.key},
		(&jose.SignerOptions{}).WithHeader("kid", "one"),
	)
	if e != nil {
		http.Error(w, "signing", 500)
		return
	}
	signed, e := jwt.Signed(signer).Claims(claims).Serialize()
	if e != nil {
		http.Error(w, "signing", 500)
		return
	}
	if err := json.NewEncoder(w).
		Encode(map[string]any{"access_token": "fixture-only", "token_type": "Bearer", "id_token": signed}); err != nil {
		t.Error(err)
	}
}

func checkOIDCGrantRevocation(
	t *testing.T,
	f *fixture,
	h *HTTP,
	p *providerFixture,
	cfg OIDCConfig,
	client *http.Client,
	origin string,
	first admin.Identity,
) {
	t.Helper()
	// Preserve the first browser session while reauthentication removes its groups.
	jar, err := cookiejar.New(nil)
	check(t, err)
	otherClient := &http.Client{Jar: jar}
	p.mu.Lock()
	p.subject = "subject-one"
	p.groups = []string{"managers"}
	p.mu.Unlock()
	if oidcLogin(t, otherClient, origin) != 200 {
		t.Fatal("second browser login")
	}
	first = httpIdentity(t, otherClient, origin)
	p.mu.Lock()
	p.groups = []string{}
	p.mu.Unlock()
	if oidcLogin(t, client, origin) != 403 {
		t.Fatal("unmatched claim login should be denied")
	}
	response, _ := request(t, otherClient, origin, "GET", "/api/session", "", nil)
	requireHTTPStatus(t, response, 401)
	// Local grants survive signed-claim reconciliation and explicit disable wins.
	check(t, f.service.ReplaceGrants(context.Background(), f.root.Token, first.ID, []admin.Grant{{Scope: f.other}}))
	if oidcLogin(t, client, origin) != 200 {
		t.Fatal("local grant removed by reconciliation")
	}
	current := httpIdentity(t, client, origin)
	if len(current.Grants) != 1 || current.Grants[0].Source != "local" {
		t.Fatal("grant provenance lost")
	}
	check(t, f.service.SetEnabled(context.Background(), f.root.Token, first.ID, false))
	if oidcLogin(t, client, origin) != 401 {
		t.Fatal("OIDC enabled disabled account")
	}
	// Mapping edits invalidate all earlier OIDC sessions, even unaffected accounts.
	cfg.Rules = cfg.Rules[:1]
	check(t, h.ConfigureOIDC(context.Background(), cfg))
	response, _ = request(t, otherClient, origin, "GET", "/api/session", "", nil)
	requireHTTPStatus(t, response, 401)
	_, err = f.service.Identity(context.Background(), f.root.Token)
	check(t, err)
}

func checkOIDCBrowserBinding(
	t *testing.T,
	f *fixture,
	h *HTTP,
	p *providerFixture,
	client *http.Client,
	origin string,
) {
	t.Helper()
	// Stop at callback and replay its URL from a different browser without binding.
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if r.URL.Path == "/auth/callback" {
			return http.ErrUseLastResponse
		}
		return nil
	}
	if oidcLogin(t, client, origin) != 302 {
		t.Fatal("did not stop at callback")
	}
	p.mu.Lock()
	callback := p.lastCallback
	p.mu.Unlock()
	other := &http.Client{}
	response, _ := request(t, other, callback, "GET", "", "", nil)
	requireHTTPStatus(t, response, 401)
	// Correct bound browser can consume it once; a wrong browser did not steal it.
	client.CheckRedirect = nil
	response, _ = request(t, client, callback, "GET", "", "", nil)
	requireHTTPStatus(t, response, 200)
	u, err := url.Parse(origin)
	check(t, err)
	cookies := client.Jar.Cookies(u)
	for _, cookie := range cookies {
		if cookie.Name == h.cookieName() && cookie.Value == "" {
			t.Fatal("empty session")
		}
	}
	check(t, f.service.DisableOIDC(context.Background()))
	response, _ = request(t, client, origin, "GET", "/api/session", "", nil)
	requireHTTPStatus(t, response, 401)
}

func (p *providerFixture) consumeTokenRequest(r *http.Request) (authorization, bool) {
	user, password, _ := r.BasicAuth()
	a, ok := p.codes[r.Form.Get("code")]
	delete(p.codes, r.Form.Get("code"))
	sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
	if !ok || user != "fry-client" || password != "fixture-client-secret" ||
		r.Form.Get("grant_type") != "authorization_code" ||
		r.Form.Get("redirect_uri") != a.redirect ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != a.challenge {
		return a, false
	}
	return a, true
}
