package main

import (
	"bytes"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCommandParsingHasIndependentState(t *testing.T) {
	var output bytes.Buffer
	first, err := parseCommand([]string{"-username", "alice", "-enabled=false", "users"}, &output)
	if err != nil || first.username != "alice" || first.enabled || first.command != "users" {
		t.Fatalf("explicit arguments lost: %+v, %v", first, err)
	}
	second, err := parseCommand([]string{"organizations"}, &output)
	if err != nil || second.username != "operator" || !second.enabled || second.command != "organizations" {
		t.Fatalf("previous invocation changed defaults: %+v, %v", second, err)
	}
}

func TestCommandParsingReportsHelpAndUsageWithoutExiting(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		help bool
	}{
		{"help", []string{"-help"}, true},
		{"unknown flag", []string{"-unknown"}, false},
		{"missing command", nil, false},
		{"extra command", []string{"users", "organizations"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			_, err := parseCommand(test.args, &output)
			if err == nil || !strings.Contains(output.String(), "Usage: fry-admin") {
				t.Fatalf("missing diagnostic: %v, %q", err, output.String())
			}
			if test.help {
				if !errors.Is(err, flag.ErrHelp) || commandExitCode(err) != 0 {
					t.Fatalf("help must succeed: %v", err)
				}
			} else if commandExitCode(err) != 2 {
				t.Fatalf("invalid arguments must be usage errors: %v", err)
			}
		})
	}
}

func TestInvalidCommandsDoNotAuthenticate(t *testing.T) {
	t.Setenv("FRY_ADMIN_PASSWORD", "disposable-test-password")
	t.Setenv("FRY_NEW_ADMIN_PASSWORD", "")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	for _, test := range []struct {
		name, want string
		args       []string
	}{
		{"unknown command", "unknown command", []string{"invented"}},
		{"missing password", "FRY_NEW_ADMIN_PASSWORD is required", []string{"user-save"}},
		{
			"invalid page", "-limit must be between",
			[]string{"-member", strings.Repeat("a", 32), "-limit", "0", "member-storage"},
		},
		{"missing output", "-out is required", []string{"download"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"-origin", server.URL}, test.args...)
			err := run(t.Context(), args)
			if err == nil || !strings.Contains(err.Error(), test.want) || commandExitCode(err) != 2 {
				t.Fatalf("input was not rejected as a usage error: %v", err)
			}
			if requests.Load() != 0 {
				t.Fatal("invalid command reached authentication")
			}
		})
	}
}
