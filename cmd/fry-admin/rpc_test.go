package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type commandWireCase struct {
	command, procedure string
	fields             map[string]any
}

func TestCLIRejectsInvalidRPCBeforeSending(t *testing.T) {
	for _, test := range []struct {
		name, method, path, want string
		data                     any
	}{
		{"malformed path", http.MethodGet, "%", "invalid URL escape", nil},
		{"unknown operation", http.MethodPost, "invented", "unknown administration operation", nil},
		{"invalid limit", http.MethodGet, "messages?limit=invalid", "invalid page limit", nil},
		{"overflowing limit", http.MethodGet, "messages?limit=2147483648", "invalid page limit", nil},
		{
			"unknown request field", http.MethodPost, "organizations", "invalid command fields",
			map[string]any{"surprise": true},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			c := &client{
				origin: "https://admin.example.test",
				http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					requests++
					return nil, errors.New("unexpected network request")
				})},
			}
			raw, err := c.call(t.Context(), test.method, test.path, test.data)
			if err == nil || !strings.Contains(err.Error(), test.want) || raw != nil || requests != 0 {
				t.Fatalf("invalid request escaped validation: error=%v result=%s requests=%d", err, raw, requests)
			}
		})
	}
}

func commandWireCases(org, inbox, id string) []commandWireCase {
	return []commandWireCase{
		{"organizations", "ListOrganizations", nil},
		{"organization-save", "PutOrganization", map[string]any{"id": id, "name": "robot"}},
		{"inboxes", "ListInboxes", map[string]any{"organization": org}},
		{"inbox-create", "CreateInbox", map[string]any{"id": id, "organization": org}},
		{"domains", "ListDomains", map[string]any{"organization": org}},
		{"domain-save", "PutDomain", map[string]any{"name": "example.test", "catch_all": inbox, "enabled": false}},
		{"aliases", "ListAliases", map[string]any{"organization": org}},
		{"alias-save", "PutAlias", map[string]any{"address": "inbox@example.test", "inboxes": []any{inbox, id}}},
		{"mail-accounts", "ListMailAccounts", map[string]any{"organization": org}},
		{"mail-account-save", "PutMailAccount", map[string]any{"username": "robot", "inbox": inbox, "enabled": false}},
		{"mail-access-set", "SetMailAccess", map[string]any{"username": "robot", "enabled": false}},
		{"member-storage", "GetMemberStorage", map[string]any{"member": id, "after": id, "limit": float64(64)}},
		{"storage-usage", "GetStorageUsage", map[string]any{"organization": org}},
		{"storage-quota-set", "SetStorageQuota", map[string]any{"limit_bytes": "9223372036854775807"}},
		{"submission-accounts", "ListSubmissionAccounts", map[string]any{"organization": org}},
		{
			"submission-account-create",
			"CreateSubmissionAccount",
			map[string]any{"authorized_domains": []any{"example.test"}},
		},
		{
			"submission-policy-set",
			"SetSubmissionPolicy",
			map[string]any{"authorized_senders": []any{"sender@example.test"}},
		},
		{
			"submission-password-set",
			"RotateSubmissionPassword",
			map[string]any{"organization": org, "username": "robot"},
		},
		{"submission-access-set", "SetSubmissionAccess", map[string]any{"organization": org, "enabled": false}},
		{"sender-grants", "ListSenderGrants", map[string]any{"organization": org}},
		{"sender-challenge", "CreateSenderChallenge", map[string]any{"scope": "example.test"}},
		{"sender-verify", "VerifySender", map[string]any{"challenge": id}},
		{"sender-revoke", "RevokeSender", map[string]any{"reason": "regression"}},
		{"sender-override", "OverrideSender", map[string]any{"reason": "regression", "scope": "example.test"}},
		{"messages", "ListMessages", map[string]any{"organization": org, "after": id}},
		{"audit", "ListAudits", map[string]any{"before": "7"}},
		{"users", "ListUsers", nil},
		{"user-save", "PutUser", map[string]any{"username": "robot", "enabled": false}},
		{"access-set", "SetEnabled", map[string]any{"user": id, "enabled": false}},
		{
			"grants-set",
			"ReplaceGrants",
			map[string]any{"user": id, "grants": []any{map[string]any{"scope": org, "content": true, "source": ""}}},
		},
	}
}

func TestCLICommandsUseTypedRPCOnWire(t *testing.T) {
	for _, name := range []string{
		"FRY_NEW_ADMIN_PASSWORD",
		"FRY_NEW_MAIL_PASSWORD",
		"FRY_NEW_SUBMISSION_PASSWORD",
	} {
		t.Setenv(name, "disposable-wire-password")
	}
	org, inbox, id := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	grants := filepath.Join(t.TempDir(), "grants.json")
	if err := os.WriteFile(grants, []byte(`[{"scope":"`+org+`","content":true}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	options := commandOptions{
		organization:      org,
		id:                id,
		name:              "robot",
		domain:            "example.test",
		address:           "inbox@example.test",
		inbox:             inbox,
		inboxes:           inbox + "," + id,
		member:            id,
		after:             id,
		limit:             64,
		before:            7,
		limitBytes:        "9223372036854775807",
		authorizedSenders: "sender@example.test",
		authorizedDomains: "example.test",
		senderScope:       "example.test",
		challenge:         id,
		reason:            "regression",
		grantsPath:        grants,
	}
	for _, test := range commandWireCases(org, inbox, id) {
		t.Run(test.command, func(t *testing.T) {
			options.command = test.command
			plan, err := options.plan()
			if err != nil {
				t.Fatal(err)
			}
			c := &client{
				origin: "https://admin.example.test",
				csrf:   "current-csrf",
				http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					assertCommandWire(t, request, test)
					return &http.Response{
						StatusCode: 200,
						Header:     http.Header{"Content-Type": {"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{}`)),
					}, nil
				})},
			}
			if _, err := c.call(t.Context(), plan.method, plan.path, plan.data); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func assertCommandWire(t *testing.T, request *http.Request, test commandWireCase) {
	t.Helper()
	if request.URL.Path != "/fry.admin.v1.AdminService/"+test.procedure || request.Method != http.MethodPost {
		t.Fatalf("incorrect typed procedure: %s %s", request.Method, request.URL.Path)
	}
	if request.Header.Get("Origin") != "https://admin.example.test" ||
		request.Header.Get("X-FRY-CSRF") != "current-csrf" {
		t.Fatal("missing native-client request protection")
	}
	var fields map[string]any
	if err := json.NewDecoder(request.Body).Decode(&fields); err != nil {
		t.Fatal(err)
	}
	if err := request.Body.Close(); err != nil {
		t.Fatal(err)
	}
	for key, want := range test.fields {
		if !reflect.DeepEqual(fields[key], want) {
			t.Errorf("field %s=%#v, want %#v", key, fields[key], want)
		}
	}
}

func TestCLIDownloadPreservesBytesAndRefusesOverwrite(t *testing.T) {
	original := "Subject: byte-exact\r\n\r\noriginal\x00bytes\r\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/content" || r.URL.Query().Get("receipt") != "receipt" {
			t.Error("wrong download target")
		}
		if _, err := io.WriteString(w, original); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	o := commandOptions{
		output:       filepath.Join(t.TempDir(), "original.eml"),
		id:           "receipt",
		organization: "org",
	}
	c := &client{origin: server.URL, http: server.Client()}
	if err := o.download(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(o.output)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != original {
		t.Fatal("original bytes changed")
	}
	stat, err := os.Stat(o.output)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0o600 {
		t.Fatal("download permissions expose content")
	}
	if err := o.download(t.Context(), c); err == nil {
		t.Fatal("download overwrote an existing original")
	}
}

func TestCLIDownloadRemovesInterruptedOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		if _, err := io.WriteString(w, "incomplete"); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	o := commandOptions{output: filepath.Join(t.TempDir(), "partial.eml")}
	if err := o.download(t.Context(), &client{origin: server.URL, http: server.Client()}); err == nil {
		t.Fatal("truncation acknowledged as complete")
	}
	if _, err := os.Stat(o.output); !os.IsNotExist(err) {
		t.Fatal("interrupted content left on disk", err)
	}
}
