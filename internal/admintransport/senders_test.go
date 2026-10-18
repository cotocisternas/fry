//go:build linux

package admintransport

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cotocisternas/fry/internal/admin"
)

func TestSenderHTTPAuthenticationAndCSRF(t *testing.T) {
	var txt string
	f := setupWithDNS(t, senderDNSFunc(func(context.Context, string) ([]string, error) { return []string{txt}, nil }))
	_, server, client := httpFixture(t, f)
	response, _ := request(t, client, server.URL, "GET", "/api/sender-grants?organization="+f.org, "", nil)
	requireHTTPStatus(t, response, 401)
	for _, path := range []string{"sender-challenge", "sender-verify", "sender-revoke", "sender-override"} {
		response, _ = request(t, client, server.URL, "POST", "/api/"+path, "", map[string]string{"organization": f.org})
		requireHTTPStatus(t, response, 401)
	}
	response, raw := request(
		t,
		client,
		server.URL,
		"POST",
		"/api/login",
		"",
		map[string]string{"username": "operator", "password": "test-operator-secret"},
	)
	requireHTTPStatus(t, response, 200)
	var who admin.Identity
	check(t, json.Unmarshal(raw, &who))
	for _, path := range []string{"sender-challenge", "sender-verify", "sender-revoke", "sender-override"} {
		response, _ = request(t, client, server.URL, "POST", "/api/"+path, "", map[string]string{"organization": f.org})
		requireHTTPStatus(t, response, 403)
	}
	response, raw = request(
		t,
		client,
		server.URL,
		"POST",
		"/api/sender-challenge",
		who.CSRF,
		map[string]string{"organization": f.org, "scope": "example.test"},
	)
	requireHTTPStatus(t, response, 200)
	var challenge admin.SenderChallenge
	check(t, json.Unmarshal(raw, &challenge))
	if !validResponseID(challenge.ID) || challenge.DNSName == "" || challenge.TXTValue == "" {
		t.Fatalf("wrong JSON %+v", challenge)
	}
	txt = challenge.TXTValue
	requireHTTPSenderVerificationAndRevocation(t, client, server.URL, f.org, who.CSRF, challenge.ID)
}

func validResponseID(raw string) bool {
	b, err := hex.DecodeString(raw)
	return err == nil && len(b) == 16 && raw == strings.ToLower(raw) && raw != strings.Repeat("0", 32)
}

func requireHTTPSenderVerificationAndRevocation(
	t *testing.T,
	client *http.Client,
	origin, organization, csrf, challengeID string,
) {
	t.Helper()
	var response responseSnapshot
	var raw []byte
	response, raw = request(
		t,
		client,
		origin,
		"POST",
		"/api/sender-verify",
		csrf,
		map[string]string{"organization": organization, "challenge": challengeID},
	)
	requireHTTPStatus(t, response, 200)
	var grant admin.SenderGrant
	check(t, json.Unmarshal(raw, &grant))
	if !grant.Active || !validResponseID(grant.ID) {
		t.Fatalf("wrong grant JSON %s", raw)
	}
	response, _ = request(
		t,
		client,
		origin,
		"POST",
		"/api/sender-override",
		csrf,
		map[string]string{"organization": organization, "scope": "other.test"},
	)
	requireHTTPStatus(t, response, 400)
	response, _ = request(
		t,
		client,
		origin,
		"POST",
		"/api/sender-revoke",
		csrf,
		map[string]string{"organization": organization, "scope": "example.test"},
	)
	requireHTTPStatus(t, response, 200)
}
