//go:build linux

package replicaconfig_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/replicaconfig"
)

// Use a live TLS endpoint and its actual PEM files. The endpoint has no member
// protocol: the wrong fingerprint must refuse the TLS connection before HTTP.
func replicaTrust(t *testing.T) (replicaconfig.TLS, *httptest.Server, *atomic.Int32) {
	t.Helper()
	requests := new(atomic.Int32)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	root, err := os.OpenRoot(t.TempDir())
	requireReplicaNoError(t, err)
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	_, otherKey, err := ed25519.GenerateKey(rand.Reader)
	requireReplicaNoError(t, err)
	otherPrivate, err := x509.MarshalPKCS8PrivateKey(otherKey)
	requireReplicaNoError(t, err)
	requireReplicaNoError(
		t,
		root.WriteFile(
			"other-key.pem",
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: otherPrivate}),
			0o600,
		),
	)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	private, err := x509.MarshalPKCS8PrivateKey(server.TLS.Certificates[0].PrivateKey)
	requireReplicaNoError(t, err)
	for name, contents := range map[string][]byte{
		"ca.pem":      certificate,
		"cert.pem":    certificate,
		"key.pem":     pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}),
		"invalid.pem": []byte("not PEM trust material\n"),
	} {
		requireReplicaNoError(t, root.WriteFile(name, contents, 0o600))
	}
	return replicaconfig.TLS{
		CAFile:          filepath.Join(root.Name(), "ca.pem"),
		CertificateFile: filepath.Join(root.Name(), "cert.pem"),
		KeyFile:         filepath.Join(root.Name(), "key.pem"),
	}, server, requests
}

func remoteStorage(t *testing.T, trust replicaconfig.TLS, server *httptest.Server) replicaconfig.Storage {
	t.Helper()
	cfg := localStorage(t)
	cfg.ReplicaTLS = &trust
	key := sha256.Sum256(server.Certificate().RawSubjectPublicKeyInfo)
	for i := range cfg.Members {
		cfg.Members[i].URL = server.URL
		cfg.Members[i].ServerKey = hex.EncodeToString(key[:])
	}
	return cfg
}

func TestStorageOpenRejectsInvalidTrustMaterial(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*replicaconfig.TLS)
		want   string
	}{
		{"missing CA", func(c *replicaconfig.TLS) { c.CAFile += ".missing" }, "replica CA"},
		{
			"invalid CA",
			func(c *replicaconfig.TLS) { c.CAFile = filepath.Join(filepath.Dir(c.CAFile), "invalid.pem") },
			"replica CA",
		},
		{"missing certificate", func(c *replicaconfig.TLS) { c.CertificateFile += ".missing" }, "replica certificate"},
		{
			"invalid certificate",
			func(c *replicaconfig.TLS) { c.CertificateFile = filepath.Join(filepath.Dir(c.CAFile), "invalid.pem") },
			"replica certificate",
		},
		{
			"mismatched private key",
			func(c *replicaconfig.TLS) { c.KeyFile = filepath.Join(filepath.Dir(c.CAFile), "other-key.pem") },
			"replica certificate",
		},
		{"missing private key", func(c *replicaconfig.TLS) { c.KeyFile += ".missing" }, "replica certificate"},
		{
			"invalid private key",
			func(c *replicaconfig.TLS) { c.KeyFile = filepath.Join(filepath.Dir(c.CAFile), "invalid.pem") },
			"replica certificate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trust, server, requests := replicaTrust(t)
			tc.change(&trust)
			cfg := remoteStorage(t, trust, server)
			store, closeStore, err := cfg.Open(t.Context(), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) || store != nil || closeStore != nil {
				t.Fatalf("invalid trust accepted: store=%v closer=%v err=%v", store, closeStore != nil, err)
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("invalid trust reached HTTP: requests=%d", got)
			}
		})
	}
}

func TestStorageOpenRejectsInvalidFingerprints(t *testing.T) {
	trust, server, requests := replicaTrust(t)
	for _, raw := range []string{
		"",
		strings.Repeat("0", 64),
		strings.Repeat("g", 64),
		strings.Repeat("1", 62),
		strings.Repeat("1", 66),
	} {
		t.Run("fingerprint_"+raw, func(t *testing.T) {
			cfg := remoteStorage(t, trust, server)
			cfg.Members[1].ServerKey = raw
			store, closeStore, err := cfg.Open(t.Context(), nil)
			if err == nil || !strings.Contains(err.Error(), "fingerprint") || store != nil || closeStore != nil {
				t.Fatalf("invalid fingerprint accepted: store=%v closer=%v err=%v", store, closeStore != nil, err)
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("invalid fingerprint reached HTTP: requests=%d", got)
			}
		})
	}
}

func TestStorageOpenRejectsWrongServerFingerprintOverTLS(t *testing.T) {
	trust, server, requests := replicaTrust(t)
	// First prove these on-disk trust files can authenticate this live endpoint.
	loaded, err := trust.Load()
	requireReplicaNoError(t, err)
	transport := &http.Transport{TLSClientConfig: loaded}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	requireReplicaNoError(t, err)
	response, err := (&http.Client{Transport: transport}).Do(request)
	requireReplicaNoError(t, err)
	requireReplicaNoError(t, response.Body.Close())
	if response.StatusCode != http.StatusNoContent || requests.Load() != 1 {
		t.Fatalf("valid TLS trust failed: status=%d requests=%d", response.StatusCode, requests.Load())
	}
	if loaded.MinVersion != tls.VersionTLS13 {
		t.Fatalf("replica TLS minimum=%d", loaded.MinVersion)
	}
	// Change only the expected SPKI: ordinary CA verification remains valid.
	cfg := remoteStorage(t, trust, server)
	wrong := sha256.Sum256([]byte("a different replica server public key"))
	cfg.Members[0].ServerKey = hex.EncodeToString(wrong[:])
	store, closeStore, err := cfg.Open(t.Context(), nil)
	if err == nil || !errors.Is(err, receiptstore.ErrStale) || store != nil || closeStore != nil {
		t.Fatalf("wrong server pin accepted: store=%v closer=%v err=%v", store, closeStore != nil, err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("wrong server pin reached HTTP: requests=%d", got)
	}
}
