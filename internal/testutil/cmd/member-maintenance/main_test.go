package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandRejectsNonLoopbackBeforeLoadingCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(
		path,
		[]byte(
			"{\"url\":\"https://example.test:443\",\"id\":\"00000000000000000000000000000001\",\""+
				"domain\":\"a\",\"server_key\":\"1111111111111111111111111111111111111111111111111111"+
				"111111111111\"}",
		),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := run(t.Context(), []string{"-config", path, "rotate"}, &out)
	if err == nil || !strings.Contains(err.Error(), "numeric loopback") || out.Len() != 0 {
		t.Fatalf("unsafe command: output=%q error=%v", out.String(), err)
	}
}

func TestCommandRejectsMalformedConfigurationWithoutPublishingReport(t *testing.T) {
	for _, tc := range []struct {
		name, contents, diagnostic string
	}{
		{"truncated JSON", `{"url":`, "unexpected EOF"},
		{"misspelled field", `{"server_keey":"pin"}`, `unknown field "server_keey"`},
		{"multiple values", `{} {}`, "configuration must contain one JSON value"},
		{"oversized input", strings.Repeat(" ", (64<<10)+1), "configuration exceeds 64 KiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err := run(t.Context(), []string{"-config", path, "rotate"}, &out)
			if err == nil || !strings.Contains(err.Error(), tc.diagnostic) {
				t.Fatalf("configuration error=%v, want %q", err, tc.diagnostic)
			}
			if out.Len() != 0 {
				t.Fatalf("invalid configuration published a report: %q", out.String())
			}
		})
	}
}
