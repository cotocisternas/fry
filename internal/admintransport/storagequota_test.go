//go:build linux

package admintransport

import (
	"encoding/json"
	"testing"

	"github.com/cotocisternas/fry/internal/admin"
)

func TestStorageQuotaHTTPExplicitNullAndCSRF(t *testing.T) {
	f := setup(t)
	_, server, client := httpFixture(t, f)
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
	response, _ := request(
		t,
		client,
		server.URL,
		"POST",
		"/api/storage-quota",
		"",
		map[string]any{"organization": f.org, "limit_bytes": "1"},
	)
	requireHTTPStatus(t, response, 403)
	for _, input := range []map[string]any{
		{"organization": f.org},
		{"organization": f.org, "limit_bytes": 1},
		{"organization": f.org, "limit_bytes": "9223372036854775808"},
	} {
		response, _ = request(t, client, server.URL, "POST", "/api/storage-quota", who.CSRF, input)
		requireHTTPStatus(t, response, 400)
	}
	for _, limit := range []any{"9007199254740993", nil} {
		response, _ = request(
			t,
			client,
			server.URL,
			"POST",
			"/api/storage-quota",
			who.CSRF,
			map[string]any{"organization": f.org, "limit_bytes": limit},
		)
		requireHTTPStatus(t, response, 200)
	}
	response, raw = request(t, client, server.URL, "GET", "/api/storage-usage?organization="+f.org, "", nil)
	requireHTTPStatus(t, response, 200)
	var usage map[string]any
	check(t, json.Unmarshal(raw, &usage))
	if usage["retained_bytes"] != "0" || usage["retained_originals"] != "0" {
		t.Fatal("not decimal strings")
	}
}
