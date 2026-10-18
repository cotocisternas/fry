package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestSubmissionCLIProcess(t *testing.T) {
	if os.Getenv("FRY_CLI_TEST_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			break
		}
	}
	if err := run(t.Context(), os.Args[1:]); err != nil {
		if _, writeErr := os.Stderr.WriteString(err.Error()); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
	os.Exit(0)
}

func TestSubmissionProvisioningCLI(t *testing.T) {
	for _, command := range []string{"submission-account-create", "submission-policy-set"} {
		t.Run(command, func(t *testing.T) {
			server, payloads := submissionTestServer(t, command)
			runSubmissionCLI(t, server.URL, command)
			assertSubmissionPayload(t, command, <-payloads)
		})
	}
}

func submissionTestServer(t *testing.T, command string) (*httptest.Server, <-chan map[string]any) {
	t.Helper()
	payloads := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/fry.admin.v1.AdminService/Login":
			if _, err := w.Write([]byte(`{"result":{"csrf":"test-csrf"}}`)); err != nil {
				t.Error(err)
			}
		case "/fry.admin.v1.AdminService/Logout":
			if _, err := w.Write([]byte(`{"ok":true}`)); err != nil {
				t.Error(err)
			}
		default:
			payloads <- decodeSubmissionRequest(t, r, command)
			if _, err := w.Write([]byte(`{"ok":true}`)); err != nil {
				t.Error(err)
			}
		}
	}))
	t.Cleanup(server.Close)
	return server, payloads
}

func decodeSubmissionRequest(t *testing.T, r *http.Request, command string) map[string]any {
	t.Helper()
	expected := "/fry.admin.v1.AdminService/SetSubmissionPolicy"
	if command == "submission-account-create" {
		expected = "/fry.admin.v1.AdminService/CreateSubmissionAccount"
	}
	if r.URL.Path != expected || r.Method != "POST" || r.Header.Get("X-FRY-CSRF") != "test-csrf" {
		t.Errorf("incorrect request: %s %s", r.Method, r.URL.Path)
	}
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Error(err)
	}
	return payload
}

func runSubmissionCLI(t *testing.T, origin, command string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// #nosec G204,G702 -- Runs only the current test executable with enumerated test commands. Owner: admin
	// maintainers; review by 2027-01-18.
	cmd := exec.CommandContext(
		t.Context(),
		executable,
		"-test.run=^TestSubmissionCLIProcess$",
		"--",
		"-origin",
		origin,
		"-organization",
		"org-a",
		"-name",
		"robot",
		"-authorized-senders",
		" One@example.test, ,Two@example.test ",
		"-authorized-domains",
		" example.test, other.test ",
		"-enabled=false",
		command,
	)
	cmd.Env = append(
		os.Environ(),
		"FRY_CLI_TEST_PROCESS=1",
		"FRY_ADMIN_PASSWORD=synthetic-admin",
		"FRY_NEW_SUBMISSION_PASSWORD=synthetic-new-secret",
	)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("CLI failed: %s", raw)
	}
	if strings.Contains(string(raw), "synthetic-new-secret") || strings.Contains(string(raw), "synthetic-admin") {
		t.Fatal("secret in CLI output")
	}
}

func assertSubmissionPayload(t *testing.T, command string, payload map[string]any) {
	t.Helper()
	if payload["organization"] != "org-a" || payload["username"] != "robot" {
		t.Fatal("identity payload incorrect")
	}
	assertPolicyArrays(t, payload)
	if command == "submission-account-create" {
		if payload["enabled"] != false || payload["password"] != "synthetic-new-secret" {
			t.Fatal("creation fields incorrect")
		}
		return
	}
	for _, key := range []string{"password", "enabled"} {
		if _, ok := payload[key]; ok {
			t.Fatal("policy transmitted unrelated field", key)
		}
	}
}

func TestSubmissionCreateRequiresEnvironmentSecret(t *testing.T) {
	t.Setenv("FRY_NEW_SUBMISSION_PASSWORD", "")
	if _, _, err := submissionProvisioning(
		"submission-account-create",
		"org",
		"robot",
		"",
		"example.test",
		true,
	); err == nil {
		t.Fatal("missing secret accepted")
	}
	if _, _, err := submissionProvisioning(
		"submission-policy-set",
		"org",
		"robot",
		"",
		"example.test",
		true,
	); err != nil {
		t.Fatal("policy requires secret")
	}
}

func assertPolicyArrays(t *testing.T, payload map[string]any) {
	t.Helper()
	for _, key := range []string{"authorized_senders", "authorized_domains"} {
		values, ok := payload[key].([]any)
		if !ok || len(values) != 2 {
			t.Fatal("scope arrays incorrect", key)
		}
	}
}
