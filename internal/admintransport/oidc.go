//go:build linux

package admintransport

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/cotocisternas/fry/internal/admin"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type OIDCConfig struct {
	Issuer, ClientID, ClientSecret        string
	Scopes, SigningAlgorithms             []string
	Rules                                 []admin.ClaimRule
	LoginLifetime, TokenMaxAge, ClockSkew time.Duration
	// LoopbackHTTP is only for an explicit local provider fixture.
	LoopbackHTTP bool
	HTTPClient   *http.Client
}
type oidcAdapter struct {
	policy   *admin.OIDCPolicy
	config   OIDCConfig
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
}

func safeEndpoint(raw string, loopback bool) bool {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	if u.Scheme == schemeHTTPS {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return loopback && u.Scheme == "http" && ip != nil && ip.IsLoopback()
}

type limitedTransport struct {
	base     http.RoundTripper
	loopback bool
}
type limitedBody struct {
	io.ReadCloser
	left int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left < 0 {
		return 0, fmt.Errorf("OIDC response exceeds limit")
	}
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		return 0, fmt.Errorf("OIDC response exceeds limit")
	}
	return n, err
}

func (t limitedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !safeEndpoint(r.URL.String(), t.loopback) {
		return nil, admin.ErrInvalid
	}
	response, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > 1<<20 {
		return nil, errors.Join(admin.ErrInvalid, response.Body.Close())
	}
	response.Body = &limitedBody{response.Body, 1 << 20}
	return response, nil
}

// ConfigureOIDC binds one installation provider and explicit signed-claim rules.
// Changing this configuration invalidates all existing OIDC sessions and pending
// login transactions. Local accounts and locally assigned grants stay intact.
func (h *HTTP) ConfigureOIDC(ctx context.Context, cfg OIDCConfig) error {
	if err := validateOIDCConfig(cfg); err != nil {
		return err
	}
	cfg.Scopes = append([]string(nil), cfg.Scopes...)
	cfg.SigningAlgorithms = append([]string(nil), cfg.SigningAlgorithms...)
	client := oidcHTTPClient(cfg)
	clientCtx := oidc.ClientContext(ctx, client)
	provider, err := discoverOIDC(clientCtx, cfg)
	if err != nil {
		return err
	}
	endpoint := provider.Endpoint()
	adapter := &oidcAdapter{
		config: cfg,
		client: client,
		verifier: provider.VerifierContext(
			clientCtx,
			&oidc.Config{ClientID: cfg.ClientID, SupportedSigningAlgs: cfg.SigningAlgorithms},
		),
	}
	adapter.oauth = oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     endpoint,
		RedirectURL:  h.origin.String() + "/auth/callback",
		Scopes:       append([]string{oidc.ScopeOpenID}, cfg.Scopes...),
	}
	policy, err := h.service.ConfigureOIDC(ctx, admin.OIDCPolicyConfig{
		Issuer:            cfg.Issuer,
		ClientID:          cfg.ClientID,
		Scopes:            cfg.Scopes,
		SigningAlgorithms: cfg.SigningAlgorithms,
		Rules:             cfg.Rules,
		LoginLifetime:     cfg.LoginLifetime,
		TokenMaxAge:       cfg.TokenMaxAge,
		ClockSkew:         cfg.ClockSkew,
	})
	if err != nil {
		return err
	}
	adapter.policy = policy
	// Rules belong to administration policy, not the network adapter.
	adapter.config.Rules = nil
	h.oidc = adapter
	return nil
}

func bindingName(h *HTTP) string {
	if h.origin.Scheme == schemeHTTPS {
		return "__Host-fry-oidc"
	}
	return "fry-oidc-local"
}

func (o *oidcAdapter) serve(h *HTTP, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/auth/oidc" {
		if err := o.start(h, w, r); err != nil {
			failure(w, err)
		}
		return
	}
	session, err := o.callback(h, r)
	// Owner: admin maintainers; review by 2027-01-18.
	// #nosec G124 -- Secure follows NewHTTP's validated origin: HTTP is allowed only on literal loopback IPs.
	http.SetCookie(
		w,
		&http.Cookie{
			Name:     bindingName(h),
			Path:     "/",
			MaxAge:   -1,
			Secure:   h.origin.Scheme == schemeHTTPS,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		},
	)
	if err != nil {
		failure(w, err)
		return
	}
	h.setSession(w, session)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (o *oidcAdapter) start(h *HTTP, w http.ResponseWriter, r *http.Request) error {
	state, err := opaque()
	if err != nil {
		return err
	}
	binding, err := opaque()
	if err != nil {
		return err
	}
	nonce, err := opaque()
	if err != nil {
		return err
	}
	verifier := oauth2.GenerateVerifier()
	if err = o.policy.CreateLogin(
		r.Context(),
		admin.OIDCLoginStart{State: state, BrowserBinding: binding, Nonce: nonce, Verifier: verifier},
	); err != nil {
		return err
	}
	// Owner: admin maintainers; review by 2027-01-18.
	// #nosec G124 -- Secure follows NewHTTP's validated origin: HTTP is allowed only on literal loopback IPs.
	http.SetCookie(
		w,
		&http.Cookie{
			Name:     bindingName(h),
			Value:    binding,
			Path:     "/",
			MaxAge:   int(o.config.LoginLifetime / time.Second),
			Secure:   h.origin.Scheme == schemeHTTPS,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		},
	)
	// Owner: admin maintainers; review by 2027-01-18.
	// #nosec G710 -- discoverOIDC validates AuthURL as HTTPS or explicitly allowed literal loopback; oauth encodes
	// state and nonce.
	http.Redirect(
		w,
		r,
		o.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)),
		http.StatusFound,
	)
	return nil
}

func (o *oidcAdapter) callback(h *HTTP, r *http.Request) (admin.Session, error) {
	login, err := o.consumeCallback(h, r)
	if err != nil {
		return admin.Session{}, err
	}
	ctx := oidc.ClientContext(r.Context(), o.client)
	identity, err := o.exchangeIdentity(ctx, r.URL.Query().Get("code"), login.Verifier)
	if err != nil {
		return admin.Session{}, err
	}
	claims, err := o.validateIdentity(identity, login.Nonce, time.Now())
	if err != nil {
		return admin.Session{}, err
	}
	return o.policy.CompleteLogin(
		r.Context(),
		login,
		admin.VerifiedOIDCIdentity{Subject: identity.Subject, Claims: claims},
	)
}

func opaque() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func oidcHTTPClient(cfg OIDCConfig) *http.Client {
	base := http.DefaultTransport
	client := &http.Client{}
	if cfg.HTTPClient != nil {
		*client = *cfg.HTTPClient
		if client.Transport != nil {
			base = client.Transport
		}
	}
	client.Transport = limitedTransport{base, cfg.LoopbackHTTP}
	client.Timeout = 15 * time.Second
	client.CheckRedirect = func(
		*http.Request,
		[]*http.Request,
	) error {
		return fmt.Errorf("OIDC endpoint redirect refused")
	}
	return client
}

func discoverOIDC(ctx context.Context, cfg OIDCConfig) (*oidc.Provider, error) {
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery failed: %w", err)
	}
	var discovery struct {
		Issuer string `json:"issuer"`
		JWKS   string `json:"jwks_uri"`
	}
	if err = provider.Claims(&discovery); err != nil {
		return nil, err
	}
	endpoint := provider.Endpoint()
	if discovery.Issuer != cfg.Issuer || !safeEndpoint(endpoint.AuthURL, cfg.LoopbackHTTP) ||
		!safeEndpoint(endpoint.TokenURL, cfg.LoopbackHTTP) ||
		!safeEndpoint(discovery.JWKS, cfg.LoopbackHTTP) {
		return nil, admin.ErrInvalid
	}
	return provider, nil
}

func validateOIDCConfig(cfg OIDCConfig) error {
	if !safeEndpoint(cfg.Issuer, cfg.LoopbackHTTP) || cfg.ClientID == "" || len(cfg.ClientID) > 1024 ||
		len(cfg.Rules) > 256 ||
		len(cfg.SigningAlgorithms) == 0 {
		return admin.ErrInvalid
	}
	issuer, err := url.Parse(cfg.Issuer)
	if err != nil || issuer.RawQuery != "" {
		return admin.ErrInvalid
	}
	if !validOIDCTiming(cfg) {
		return admin.ErrInvalid
	}
	return validateOIDCAlgorithms(cfg.SigningAlgorithms)
}

func validOIDCTiming(cfg OIDCConfig) bool {
	return cfg.LoginLifetime >= time.Minute && cfg.LoginLifetime <= 10*time.Minute && cfg.TokenMaxAge >= time.Minute &&
		cfg.TokenMaxAge <= 15*time.Minute &&
		cfg.ClockSkew >= 0 &&
		cfg.ClockSkew <= time.Minute
}

func validateOIDCAlgorithms(algorithms []string) error {
	allowed := map[string]bool{
		"RS256": true,
		"RS384": true,
		"RS512": true,
		"ES256": true,
		"ES384": true,
		"ES512": true,
		"PS256": true,
		"PS384": true,
		"PS512": true,
		"EdDSA": true,
	}
	for _, algorithm := range algorithms {
		if !allowed[algorithm] {
			return admin.ErrInvalid
		}
	}
	return nil
}

func (o *oidcAdapter) consumeCallback(h *HTTP, r *http.Request) (admin.OIDCLogin, error) {
	query := r.URL.Query()
	if len(query["state"]) != 1 || len(query.Get("state")) != 43 {
		return admin.OIDCLogin{}, admin.ErrUnauthorized
	}
	cookie, err := r.Cookie(bindingName(h))
	if err != nil || len(cookie.Value) != 43 {
		return admin.OIDCLogin{}, admin.ErrUnauthorized
	}
	// Consume state before exchange, including provider error callbacks.
	login, err := o.policy.ConsumeLogin(r.Context(), query.Get("state"), cookie.Value)
	if err != nil {
		return login, err
	}
	if query.Get("error") != "" || len(query["code"]) != 1 || query.Get("code") == "" || len(query.Get("code")) > 4096 {
		return login, admin.ErrUnauthorized
	}
	return login, nil
}

func (o *oidcAdapter) exchangeIdentity(ctx context.Context, code, verifier string) (*oidc.IDToken, error) {
	tokens, err := o.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, admin.ErrUnauthorized
	}
	raw, ok := tokens.Extra("id_token").(string)
	if !ok || len(raw) > 128<<10 {
		return nil, admin.ErrUnauthorized
	}
	identity, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, admin.ErrUnauthorized
	}
	return identity, nil
}

func (o *oidcAdapter) validateIdentity(
	identity *oidc.IDToken,
	nonce string,
	now time.Time,
) (map[string]json.RawMessage, error) {
	if identity.Issuer != o.config.Issuer || identity.Subject == "" || len(identity.Subject) > 1024 {
		return nil, admin.ErrUnauthorized
	}
	if !o.validTokenTime(identity, now) || subtle.ConstantTimeCompare([]byte(identity.Nonce), []byte(nonce)) != 1 {
		return nil, admin.ErrUnauthorized
	}
	var claims map[string]json.RawMessage
	if err := identity.Claims(&claims); err != nil {
		return nil, admin.ErrUnauthorized
	}
	if !validNotBefore(claims, now.Add(o.config.ClockSkew)) ||
		!validAuthorizedParty(claims, o.config.ClientID, len(identity.Audience)) {
		return nil, admin.ErrUnauthorized
	}
	return claims, nil
}

func (o *oidcAdapter) validTokenTime(identity *oidc.IDToken, now time.Time) bool {
	return !identity.IssuedAt.IsZero() && !identity.IssuedAt.After(now.Add(o.config.ClockSkew)) &&
		!identity.IssuedAt.Before(now.Add(-o.config.TokenMaxAge-o.config.ClockSkew)) &&
		!identity.Expiry.IsZero() &&
		identity.Expiry.After(identity.IssuedAt)
}

func validNotBefore(claims map[string]json.RawMessage, latest time.Time) bool {
	// Honor the configured skew rather than the library's five-minute nbf leeway.
	raw, exists := claims["nbf"]
	if !exists {
		return true
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	seconds, ok := value.(float64)
	return ok && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds <= float64(latest.UnixNano())/1e9
}

func validAuthorizedParty(claims map[string]json.RawMessage, client string, audiences int) bool {
	raw, exists := claims["azp"]
	if !exists {
		return audiences <= 1
	}
	var party string
	return json.Unmarshal(raw, &party) == nil && party == client
}
