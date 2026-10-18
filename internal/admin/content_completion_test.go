//go:build linux

package admin

import (
	"bytes"
	"io"
	"testing"
)

func TestContentAuditRequiresVerifiedEOF(t *testing.T) {
	f := setup(t)
	receipt, original := f.receive(t)
	content, err := f.service.OpenContent(t.Context(), f.root.Token, f.org, receipt)
	check(t, err)
	t.Cleanup(func() { check(t, content.Close()) })
	// Consuming the advertised size does not observe the terminal verification
	// result. A future size-limited caller must not claim a verified transfer.
	body := make([]byte, len(original))
	_, err = io.ReadFull(content, body)
	check(t, err)
	if !bytes.Equal(body, original) {
		t.Fatal("original bytes changed")
	}
	check(t, content.Close())
	events, err := f.service.Audits(t.Context(), f.root.Token, 0)
	check(t, err)
	for _, event := range events {
		if event.Action == "content-download" && event.Target == receipt {
			if event.Outcome != "interrupted" {
				t.Fatalf("size-only read audit = %q; want interrupted", event.Outcome)
			}
			return
		}
	}
	t.Fatal("content audit missing")
}
