//go:build linux

package admintransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cotocisternas/fry/internal/admin"
)

func httpFixture(t *testing.T, f *fixture) (*HTTP, *httptest.Server, *http.Client) {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	handler, err := NewHTTP(f.service, HTTPOptions{Origin: "http://" + server.Listener.Addr().String()})
	check(t, err)
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	check(t, err)
	return handler, server, &http.Client{Jar: jar}
}

// responseSnapshot contains metadata only; request owns and closes the body.
type responseSnapshot struct {
	StatusCode int
	Status     string
	Header     http.Header
}

func (r responseSnapshot) Cookies() []*http.Cookie {
	return (&http.Response{Header: r.Header}).Cookies()
}

func request(t *testing.T, c *http.Client, origin, method, path, csrf string, body any) (responseSnapshot, []byte) {
	t.Helper()
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		check(t, err)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, origin+path, bytes.NewReader(b))
	check(t, err)
	req.Header.Set("Origin", origin)
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-FRY-CSRF", csrf)
	}
	response, err := c.Do(req)
	check(t, err)
	defer func() { check(t, response.Body.Close()) }()
	result, err := io.ReadAll(response.Body)
	check(t, err)
	return responseSnapshot{response.StatusCode, response.Status, response.Header.Clone()}, result
}

func TestHTTPAuthenticationCSRFAndAuditedDownload(t *testing.T) {
	f := setup(t)
	_, server, client := httpFixture(t, f)
	response, _ := request(t, client, server.URL, "GET", "/api/organizations", "", nil)
	requireHTTPStatus(t, response, 401)
	rejectCrossOriginLogin(t, client, server.URL)
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
	if len(response.Cookies()) != 1 || !response.Cookies()[0].HttpOnly ||
		response.Cookies()[0].SameSite != http.SameSiteLaxMode {
		t.Fatal("unsafe cookie")
	}
	response, _ = request(
		t,
		client,
		server.URL,
		"POST",
		"/api/organizations",
		"",
		admin.Organization{ID: newID(t), Name: "CSRF rejected"},
	)
	requireHTTPStatus(t, response, 403)
	response, _ = request(
		t,
		client,
		server.URL,
		"POST",
		"/api/organizations",
		who.CSRF,
		admin.Organization{ID: newID(t), Name: "New organization"},
	)
	requireHTTPStatus(t, response, 200)
	requireHTTPExactContentDownload(t, f, client, server.URL)
	response, raw = request(t, client, server.URL, "GET", "/", "", nil)
	if response.StatusCode != 200 || !bytes.Contains(raw, []byte("Keep the mail moving")) ||
		!strings.Contains(response.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("web shell or security headers missing")
	}
	response, _ = request(t, client, server.URL, "POST", "/api/logout", who.CSRF, map[string]bool{})
	requireHTTPStatus(t, response, 200)
	response, _ = request(t, client, server.URL, "GET", "/api/session", "", nil)
	requireHTTPStatus(t, response, 401)
	requireHTTPManagerCannotCreateUsers(t, f, client, server.URL)
}

// A writer failure can happen after the reader has buffered the entire original.
// Audit completion must reflect the transfer, not just consumption of the reader.
func TestHTTPInterruptedDownloadIsNotAuditedAsComplete(t *testing.T) {
	f := setup(t)
	receipt, _ := f.receive(t)
	handler, err := NewHTTP(f.service, HTTPOptions{Origin: "http://127.0.0.1:8080"})
	check(t, err)
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"http://127.0.0.1:8080/api/content?organization="+f.org+"&receipt="+receipt,
		nil,
	)
	req.AddCookie(
		&http.Cookie{
			Name:     "fry-admin-local",
			Value:    f.root.Token,
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		},
	)
	writer := &failedDownloadWriter{header: make(http.Header)}
	requireInterruptedDownload(t, handler, writer, req)
	if writer.writes != 1 {
		t.Fatalf("download write attempts = %d, want 1", writer.writes)
	}
	events, err := f.service.Audits(context.Background(), f.root.Token, 0)
	check(t, err)
	for _, event := range events {
		if event.Action == "content-download" && event.Target == receipt {
			if event.Outcome != "interrupted" {
				t.Fatalf("failed transfer audit = %q, want interrupted", event.Outcome)
			}
			return
		}
	}
	t.Fatal("missing download audit")
}

type failedDownloadWriter struct {
	header http.Header
	writes int
}

func (w *failedDownloadWriter) Header() http.Header { return w.header }
func (w *failedDownloadWriter) WriteHeader(int)     {}
func (w *failedDownloadWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, errors.New("client disconnected")
}

func requireHTTPStatus(t *testing.T, response responseSnapshot, want int) {
	t.Helper()
	if response.StatusCode != want {
		t.Fatalf("HTTP status=%d (%s), want %d", response.StatusCode, response.Status, want)
	}
}

func requirePrivateResponse(t *testing.T, raw []byte, forbidden ...string) {
	t.Helper()
	for _, value := range forbidden {
		if bytes.Contains(raw, []byte(value)) {
			t.Fatalf("response disclosed forbidden %q", value)
		}
	}
}

func rejectCrossOriginLogin(t *testing.T, client *http.Client, origin string) {
	t.Helper()
	req, err := http.NewRequestWithContext(
		t.Context(),
		"POST",
		origin+"/api/login",
		strings.NewReader(`{"username":"operator","password":"test-operator-secret"}`),
	)
	check(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://attacker.test")
	rawResponse, err := client.Do(req)
	check(t, err)
	check(t, rawResponse.Body.Close())
	if rawResponse.StatusCode != 403 {
		t.Fatal("unexpected HTTP status", rawResponse.StatusCode)
	}
}

func requireInterruptedDownload(t *testing.T, handler http.Handler, writer http.ResponseWriter, req *http.Request) {
	t.Helper()
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				recoveredErr, ok := recovered.(error)
				if !ok || !errors.Is(recoveredErr, http.ErrAbortHandler) {
					t.Fatalf("interrupted response panic = %v, want ErrAbortHandler", recovered)
				}
			} else {
				t.Fatal("expected ErrAbortHandler")
			}
		}()
		handler.ServeHTTP(writer, req)
	}()
}

func requireHTTPManagerCannotCreateUsers(t *testing.T, f *fixture, client *http.Client, origin string) {
	t.Helper()
	var who admin.Identity
	// A manager cannot reset accounts or grant roles through the HTTP seam.
	manager := f.manager(t, false)
	check(t, f.service.Logout(context.Background(), manager.Token))
	response, raw := request(
		t,
		client,
		origin,
		"POST",
		"/api/login",
		"",
		map[string]string{"username": "manager", "password": "test-manager-secret"},
	)
	requireHTTPStatus(t, response, 200)
	check(t, json.Unmarshal(raw, &who))
	response, _ = request(
		t,
		client,
		origin,
		"POST",
		"/api/users",
		who.CSRF,
		map[string]any{"username": "intruder", "password": "test-password", "enabled": true},
	)
	requireHTTPStatus(t, response, 403)
}

func requireHTTPExactContentDownload(t *testing.T, f *fixture, client *http.Client, origin string) {
	t.Helper()
	receipt, message := f.receive(t)
	response, raw := request(
		t,
		client,
		origin,
		"GET",
		"/api/content?organization="+f.org+"&receipt="+receipt,
		"",
		nil,
	)
	if response.StatusCode != 200 || !bytes.Equal(raw, message) ||
		!strings.HasPrefix(response.Header.Get("Content-Disposition"), "attachment") {
		t.Fatal("download was not exact safe attachment")
	}
}
