//go:build linux

package admintransport

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/admin"
)

func TestMemberStorageHTTPBoundsAndOperatorScope(t *testing.T) {
	f := setup(t)
	member := f.member
	_, server, client := httpFixture(t, f)
	client.Timeout = 5 * time.Second
	response, _ := request(t, client, server.URL, "GET", "/api/member-storage?member="+member, "", nil)
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
	requireMemberStorageHTTPBoundsAndPrivacy(t, client, server.URL, member)
	response, _ = request(
		t,
		client,
		server.URL,
		"POST",
		"/api/member-storage",
		who.CSRF,
		map[string]string{"member": member},
	)
	requireHTTPStatus(t, response, 404)
	manager := f.manager(t, false)
	check(
		t,
		f.service.ReplaceGrants(
			context.Background(),
			f.root.Token,
			manager.Identity.ID,
			[]admin.Grant{{Scope: f.org}, {Scope: f.other}},
		),
	)
	_, server2, client2 := httpFixture(t, f)
	client2.Timeout = 5 * time.Second
	request(
		t,
		client2,
		server2.URL,
		"POST",
		"/api/login",
		"",
		map[string]string{"username": "manager", "password": "test-manager-secret"},
	)
	response, raw = request(t, client2, server2.URL, "GET", "/api/member-storage?member="+member, "", nil)
	if response.StatusCode != 403 || strings.Contains(string(raw), member) {
		t.Fatal("multi-organization manager received installation topology", response.Status)
	}
}

func requireMemberStorageHTTPBoundsAndPrivacy(t *testing.T, client *http.Client, origin, member string) {
	t.Helper()
	var response responseSnapshot
	var raw []byte
	for _, query := range []string{
		"",
		"member=bad",
		"member=" + strings.Repeat("0", 32),
		"member=" + member + "&after=bad",
		"member=" + member + "&limit=0",
		"member=" + member + "&limit=65",
		"member=" + member + "&limit=-1",
		"member=" + member + "&limit=1e1",
	} {
		response, _ = request(t, client, origin, "GET", "/api/member-storage?"+query, "", nil)
		requireHTTPStatus(t, response, 400)
	}
	response, _ = request(t, client, origin, "GET", "/api/member-storage?member="+newID(t), "", nil)
	requireHTTPStatus(t, response, 404)
	response, raw = request(t, client, origin, "GET", "/api/member-storage?member="+member+"&limit=1", "", nil)
	requireHTTPStatus(t, response, 200)
	var snapshot map[string]any
	check(t, json.Unmarshal(raw, &snapshot))
	profile, ok := snapshot["profile"].(map[string]any)
	if !ok {
		t.Fatal("missing profile")
	}
	if _, ok = profile["max_bytes"].(string); !ok {
		t.Fatal("64-bit capacity was not a decimal string")
	}
	if strings.Contains(string(raw), "directory") || strings.Contains(string(raw), "endpoint") ||
		strings.Contains(string(raw), "http://") {
		t.Fatal("diagnostics exposed physical paths or transport endpoints")
	}
}
