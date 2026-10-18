//go:build linux

package main

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestAdministrationServesHTTP2(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "loopback"
		if encrypted {
			name = "tls"
		}
		t.Run(name, func(t *testing.T) { requireAdministrationHTTP2(t, encrypted) })
	}
}

func requireAdministrationHTTP2(t *testing.T, encrypted bool) {
	t.Helper()
	cfg := transportConfig{Transport: "loopback"}
	transport := &http.Transport{ForceAttemptHTTP2: true}
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP2(true)
	transport.Protocols.SetUnencryptedHTTP2(!encrypted)
	scheme := "http"
	if encrypted {
		var roots *tls.Config
		cfg, roots = administrationTLSClient(t)
		transport.TLSClientConfig = roots
		scheme = "https"
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	failures := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.ProtoMajor != 2 {
			t.Error("administration handler did not receive HTTP/2")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	stop := serveAdministration(
		t.Context(),
		listener,
		handler,
		&adminConfig{transportConfig: cfg, MaxConnections: 2},
		failures,
	)
	defer stop()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	requireHTTP2Response(t, client, scheme+"://"+listener.Addr().String())
}

func administrationTLSClient(t *testing.T) (transportConfig, *tls.Config) {
	t.Helper()
	cfg, roots := listenerCertificate(t)
	if err := cfg.prepare("127.0.0.1:0", "administration"); err != nil {
		t.Fatal(err)
	}
	return cfg, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
}

func requireHTTP2Response(t *testing.T, client *http.Client, address string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("response body: %v %v", readErr, closeErr)
	}
	if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("response: %s %s", resp.Proto, resp.Status)
	}
}
