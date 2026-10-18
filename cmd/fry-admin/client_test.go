package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAdministrationRequestUsesCommandContext(t *testing.T) {
	type contextKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), contextKey{}, "command"))
	defer cancel()
	c := &client{
		origin: "http://127.0.0.1",
		http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Context().Value(contextKey{}) != "command" {
				t.Error("request lost command context")
			}
			cancel()
			return nil, r.Context().Err()
		})},
	}
	if _, err := c.call(ctx, http.MethodGet, "organizations", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was not propagated: %v", err)
	}
}

type failingCloseBody struct {
	io.Reader
	closed bool
}

func (b *failingCloseBody) Close() error {
	b.closed = true
	return errors.New("private response transport detail")
}

func TestAcknowledgedMutationSurvivesResponseCleanupFailure(t *testing.T) {
	body := &failingCloseBody{
		Reader: strings.NewReader(`{"result":{"id":"org-id","name":"test"}}`),
	}
	calls := 0
	c := &client{
		origin: "http://127.0.0.1",
		http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       body,
				Header:     http.Header{"Content-Type": {"application/json"}},
			}, nil
		})},
	}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	raw, err := c.call(t.Context(), http.MethodPost, "organizations", map[string]string{"name": "test"})
	if err != nil || !json.Valid(raw) || !body.closed || calls != 1 {
		t.Fatalf(
			"acknowledged result changed by cleanup: raw=%s err=%v closed=%v calls=%d",
			raw,
			err,
			body.closed,
			calls,
		)
	}
	if !strings.Contains(output.String(), "could not close administration response") ||
		strings.Contains(output.String(), "private response transport detail") {
		t.Fatal("cleanup diagnostic was missing or exposed private details")
	}
}
