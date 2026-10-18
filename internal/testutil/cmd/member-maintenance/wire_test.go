package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/replicaconfig"

	memberv1 "github.com/cotocisternas/fry/gen/member/go/fry/member/v1"
	memberconnect "github.com/cotocisternas/fry/gen/member/go/fry/member/v1/memberv1connect"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
)

const (
	fixtureMember = "00000000000000000000000000000001"
	fixtureBefore = "00000000000000000000000000000002"
	fixtureAfter  = "00000000000000000000000000000003"
)

type wirePeer struct {
	mu            sync.Mutex
	current       string
	release       string
	releaseSuffix string
	outcome       string
	requests      []*memberv1.MaintenanceRequest
}

func (p *wirePeer) identity(_ context.Context, _ connect.Spec, stream connect.ServerStream) error {
	if err := stream.Receive(&memberv1.IdentityRequest{}); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return stream.Send(
		&memberv1.IdentityResponse{
			Member:        fixtureMember,
			Incarnation:   p.current,
			FailureDomain: "fixture",
			OwnerEpoch:    7,
			Release:       p.release + p.releaseSuffix,
		},
	)
}

func (p *wirePeer) maintenance(_ context.Context, _ connect.Spec, stream connect.ServerStream) error {
	request := &memberv1.MaintenanceRequest{}
	if err := stream.Receive(request); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, request)
	if request.Kind == memberv1.MaintenanceKind_MAINTENANCE_KIND_ROTATE {
		p.current = fixtureAfter
	}
	state := p.outcome
	if state == "" {
		state = "complete"
	}
	return stream.Send(&memberv1.MaintenanceResponse{State: state})
}

func tlsFiles(t *testing.T) (replicaconfig.TLS, *tls.Config, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(
		certPath,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		keyPath,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	fingerprint := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
	return replicaconfig.TLS{
		CAFile:          certPath,
		CertificateFile: certPath,
		KeyFile:         keyPath,
	}, &tls.Config{
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    roots,
		Certificates: []tls.Certificate{{Certificate: [][]byte{raw}, PrivateKey: private}},
	}, hex.EncodeToString(
		fingerprint[:],
	)
}

func peerFixture(t *testing.T) (string, *wirePeer) {
	t.Helper()
	peer := &wirePeer{current: fixtureBefore}
	rpc := connect.NewServer()
	rpc.Register(
		connect.Method{
			Spec: connect.Spec{
				Procedure:  memberconnect.MemberServiceIdentityProcedure,
				StreamType: connect.StreamTypeUnary,
			},
			Handler: peer.identity,
		},
	)
	rpc.Register(
		connect.Method{
			Spec: connect.Spec{
				Procedure:  memberconnect.MemberServiceMaintenanceProcedure,
				StreamType: connect.StreamTypeUnary,
			},
			Handler: peer.maintenance,
		},
	)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, rpc)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 || r.Header.Get("Content-Type") != "application/grpc" ||
			!strings.HasPrefix(r.Header.Get("Fry-Release"), "prototype-member-grpc-v1") {
			http.Error(w, "wrong member protocol or release", http.StatusPreconditionFailed)
			return
		}
		peer.mu.Lock()
		peer.release = r.Header.Get("Fry-Release")
		peer.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	trust, tlsConfig, pin := tlsFiles(t)
	server.TLS = tlsConfig
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "member.json")
	data, err := json.Marshal(
		config{
			MemberConfig: replicaconfig.MemberConfig{
				URL:       server.URL,
				ID:        fixtureMember,
				Domain:    "fixture",
				ServerKey: pin,
			},
			TLS: trust,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, peer
}

func TestCommandRunsAuthenticatedRotationStatusAndReplay(t *testing.T) {
	path, peer := peerFixture(t)
	var out bytes.Buffer
	if err := run(t.Context(), []string{"-config", path, "rotate"}, &out); err != nil {
		t.Fatal(err)
	}
	checkRotationReport(t, out.Bytes())
	checkMaintenanceRequests(t, peer, memberv1.MaintenanceKind_MAINTENANCE_KIND_ROTATE)
}

func checkRotationReport(t *testing.T, data []byte) {
	t.Helper()
	var value struct {
		Before, After struct {
			Member, Incarnation string
			OwnerEpoch          int64 `json:"owner_epoch"`
		}
		States []string
	}
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if value.Before.Member != fixtureMember || value.Before.Incarnation != fixtureBefore ||
		value.After.Incarnation != fixtureAfter {
		t.Fatalf("incorrect identities: %s", data)
	}
	if value.Before.OwnerEpoch != 7 || value.After.OwnerEpoch != 7 ||
		fmt.Sprint(value.States) != "[complete complete complete]" {
		t.Fatalf("incorrect outcomes: %s", data)
	}
}

func checkMaintenanceRequests(t *testing.T, peer *wirePeer, kind memberv1.MaintenanceKind) {
	t.Helper()
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if len(peer.requests) != 3 {
		t.Fatalf("maintenance calls=%d", len(peer.requests))
	}
	operation := peer.requests[0].Operation
	if len(operation) != 32 {
		t.Fatalf("invalid operation %q", operation)
	}
	for i, request := range peer.requests {
		if !matchesMaintenanceRequest(request, operation, kind, i == 1) {
			t.Fatalf("operation/status/replay changed: %+v", request)
		}
	}
}

func matchesMaintenanceRequest(
	request *memberv1.MaintenanceRequest,
	operation string,
	kind memberv1.MaintenanceKind,
	status bool,
) bool {
	return request.Operation == operation && request.Target.Incarnation == fixtureBefore &&
		request.Target.OwnerEpoch == 7 &&
		request.Kind == kind &&
		request.StatusOnly == status
}

func TestCommandRunsSealedReclamationWithoutChangingWriterIdentity(t *testing.T) {
	path, peer := peerFixture(t)
	var out bytes.Buffer
	if err := run(t.Context(), []string{"-config", path, "reclaim-sealed"}, &out); err != nil {
		t.Fatal(err)
	}
	var value struct {
		Before, After json.RawMessage
		States        []string
	}
	if err := json.Unmarshal(out.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(value.Before, value.After) || fmt.Sprint(value.States) != "[complete complete complete]" {
		t.Fatalf("sealed reclaim changed writer: %s", out.String())
	}
	checkMaintenanceRequests(t, peer, memberv1.MaintenanceKind_MAINTENANCE_KIND_SEALED_RECLAIM)
}

func TestCommandRefusesMismatchedPeerBeforeMaintenance(t *testing.T) {
	for _, mismatch := range []string{"pin", "member", "domain", "release"} {
		t.Run(mismatch, func(t *testing.T) {
			path, peer := peerFixture(t)
			mutatePeer(t, path, peer, mismatch)
			var out bytes.Buffer
			if err := run(t.Context(), []string{"-config", path, "rotate"}, &out); err == nil || out.Len() != 0 {
				t.Fatalf("mismatched peer reported success: %q %v", out.String(), err)
			}
			peer.mu.Lock()
			defer peer.mu.Unlock()
			if len(peer.requests) != 0 {
				t.Fatal("maintenance sent to mismatched peer")
			}
		})
	}
}

func mutatePeer(t *testing.T, path string, peer *wirePeer, mismatch string) {
	t.Helper()
	// Local test-owned configuration; owner FRY maintainers, review 2027-01-18.
	//nolint:gosec // Reviewed call; rationale above.
	raw, err := os.ReadFile(
		path,
	)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	switch mismatch {
	case "pin":
		cfg.ServerKey = strings.Repeat("11", 32)
	case "member":
		cfg.ID = "00000000000000000000000000000009"
	case "domain":
		cfg.Domain = "other"
	case "release":
		peer.releaseSuffix = "-other"
	}
	raw, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCommandDoesNotPublishIncompleteMaintenance(t *testing.T) {
	path, peer := peerFixture(t)
	peer.outcome = "running"
	var out bytes.Buffer
	err := run(t.Context(), []string{"-config", path, "rotate"}, &out)
	if err == nil || !strings.Contains(err.Error(), "not complete") || out.Len() != 0 {
		t.Fatalf("incomplete operation reported success: %q %v", out.String(), err)
	}
}

func TestCommandRejectsMissingClientKeyBeforeMaintenance(t *testing.T) {
	path, peer := peerFixture(t)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.TLS.KeyFile = filepath.Join(t.TempDir(), "missing-client-key.pem")
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = run(t.Context(), []string{"-config", path, "rotate"}, &out)
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "replica certificate") {
		t.Fatalf("missing client key error=%v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("missing client key published a report: %q", out.String())
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if len(peer.requests) != 0 {
		t.Fatal("maintenance sent without client credentials")
	}
}
