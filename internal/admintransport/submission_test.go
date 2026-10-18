//go:build linux

package admintransport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cotocisternas/fry/internal/admin"
	"github.com/cotocisternas/fry/internal/maildirectory"
)

func TestSubmissionHTTPPrivacyAndCSRF(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	directory := maildirectory.New(f.db)
	check(
		t,
		directory.SeedSubmission(
			ctx,
			[]maildirectory.SubmissionAccount{
				{
					Username:          "sender",
					Organization:      f.org,
					Enabled:           true,
					AuthorizedDomains: []string{"business.test", "example.test"},
				},
			},
			[]string{"initial-secret"},
		),
	)
	_, server, client := httpFixture(t, f)
	response, _ := request(t, client, server.URL, "GET", "/api/submission-accounts?organization="+f.org, "", nil)
	requireHTTPStatus(t, response, 401)
	_, raw := request(
		t,
		client,
		server.URL,
		"POST",
		"/api/login",
		"",
		map[string]string{"username": "operator", "password": "test-operator-secret"},
	)
	var who admin.Identity
	check(t, json.Unmarshal(raw, &who))
	requireHTTPSubmissionRotationPrivacy(t, client, server.URL, f.org, who.CSRF)
	requireHTTPSubmissionDisable(t, client, server.URL, f.org, who.CSRF, directory)
}

func TestSubmissionCreateAndPolicyHTTP(t *testing.T) {
	f := setup(t)
	_, server, client := httpFixture(t, f)
	payload := map[string]any{
		"organization":       f.org,
		"username":           "http-dynamic",
		"password":           "http-private-secret",
		"enabled":            true,
		"authorized_senders": []string{},
		"authorized_domains": []string{"example.test"},
	}
	response, _ := request(t, client, server.URL, "POST", "/api/submission-accounts", "", payload)
	requireHTTPStatus(t, response, 401)
	_, raw := request(
		t,
		client,
		server.URL,
		"POST",
		"/api/login",
		"",
		map[string]string{"username": "operator", "password": "test-operator-secret"},
	)
	var who admin.Identity
	check(t, json.Unmarshal(raw, &who))
	for _, path := range []string{"submission-accounts", "submission-policy"} {
		response, _ = request(t, client, server.URL, "POST", "/api/"+path, "", payload)
		requireHTTPStatus(t, response, 403)
	}
	response, _ = request(t, client, server.URL, "POST", "/api/submission-accounts", who.CSRF, payload)
	requireHTTPStatus(t, response, 200)
	policy := map[string]any{
		"organization":       f.org,
		"username":           "http-dynamic",
		"authorized_senders": []string{"Exact@Example.TEST"},
		"authorized_domains": []string{},
	}
	response, _ = request(t, client, server.URL, "POST", "/api/submission-policy", who.CSRF, policy)
	requireHTTPStatus(t, response, 200)
	response, raw = request(t, client, server.URL, "GET", "/api/submission-accounts?organization="+f.org, "", nil)
	if response.StatusCode != 200 || !strings.Contains(string(raw), "Exact@example.test") ||
		strings.Contains(string(raw), "secret") ||
		strings.Contains(string(raw), "hash") {
		t.Fatal(response.Status, string(raw))
	}
}

func requireHTTPSubmissionRotationPrivacy(t *testing.T, client *http.Client, origin, organization, csrf string) {
	t.Helper()
	var response responseSnapshot
	var raw []byte
	for _, endpoint := range []string{"submission-password", "submission-access"} {
		response, _ = request(
			t,
			client,
			origin,
			"POST",
			"/api/"+endpoint,
			"",
			map[string]any{
				"organization": organization,
				"username":     "sender",
				"password":     "new-send-secret",
				"enabled":      false,
			},
		)
		requireHTTPStatus(t, response, 403)
	}
	response, raw = request(
		t,
		client,
		origin,
		"POST",
		"/api/submission-password",
		csrf,
		map[string]string{"organization": organization, "username": "sender", "password": "new-send-secret"},
	)
	if response.StatusCode != 200 || strings.Contains(string(raw), "secret") {
		t.Fatalf("rotation response: %s %s", response.Status, raw)
	}
	response, raw = request(t, client, origin, "GET", "/api/submission-accounts?organization="+organization, "", nil)
	requireHTTPStatus(t, response, 200)
	requirePrivateResponse(t, raw, "password", "secret", "pbkdf2")
	var accounts []maildirectory.SubmissionAccount
	check(t, json.Unmarshal(raw, &accounts))
	if len(accounts) != 1 || accounts[0].Version != 2 {
		t.Fatalf("wrong listing: %+v", accounts)
	}
}

func requireHTTPSubmissionDisable(
	t *testing.T,
	client *http.Client,
	origin, organization, csrf string,
	directory *maildirectory.Directory,
) {
	t.Helper()
	var response responseSnapshot
	response, _ = request(
		t,
		client,
		origin,
		"POST",
		"/api/submission-access",
		csrf,
		map[string]any{"organization": organization, "username": "sender", "enabled": false},
	)
	requireHTTPStatus(t, response, 200)
	if _, err := directory.AuthenticateSubmission(
		context.Background(),
		"sender",
		"new-send-secret",
	); !errors.Is(
		err,
		maildirectory.ErrNotFound,
	) {
		t.Fatal("HTTP disable ineffective", err)
	}
	response, _ = request(
		t,
		client,
		origin,
		"POST",
		"/api/submission-password",
		csrf,
		map[string]string{"organization": organization, "username": "missing", "password": "new-send-secret"},
	)
	requireHTTPStatus(t, response, 404)
}
