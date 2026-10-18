package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/cotocisternas/fry/internal/replicaconfig"

	memberv1 "github.com/cotocisternas/fry/gen/member/go/fry/member/v1"
	memberconnect "github.com/cotocisternas/fry/gen/member/go/fry/member/v1/memberv1connect"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
)

// The helper and member are built together from the same root module. Keep the
// private release identity identical to receiptstore.memberRelease; a mismatch
// is a failed rehearsal, never permission to weaken the server's policy.
func buildRelease() string {
	result := "prototype-member-grpc-v1"
	if info, ok := debug.ReadBuildInfo(); ok {
		result += "/" + info.Main.Version
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				result += "/" + setting.Value
			}
			if setting.Key == "vcs.modified" {
				result += "/dirty=" + setting.Value
			}
		}
	}
	return result
}

type memberHTTP struct{ client *http.Client }

func (c memberHTTP) Do(request *http.Request) (*http.Response, error) {
	request.Header.Set("Fry-Release", buildRelease())
	// G704: numeric loopback fixture, pinned mTLS, fixed generated RPCs, no proxy/redirects; owner FRY
	// maintainers, review 2027-01-18.
	//nolint:gosec // Reviewed call; rationale above.
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.ProtoMajor != 2 || !validPeer(response.TLS) {
		return nil, errors.Join(
			fmt.Errorf("member requires current mutually authenticated HTTP/2 TLS"),
			response.Body.Close(),
		)
	}
	return response, nil
}

func validPeer(state *tls.ConnectionState) bool {
	if state == nil || len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return false
	}
	now := time.Now()
	for _, certificate := range state.VerifiedChains[0] {
		if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
			return false
		}
	}
	return true
}

func client(cfg config) (memberconnect.MemberServiceClient, func(), error) {
	tc, err := cfg.TLS.Load()
	if err != nil {
		return nil, nil, err
	}
	pin, err := replicaconfig.Key(cfg.ServerKey)
	if err != nil {
		return nil, nil, err
	}
	tc.NextProtos = []string{"h2"}
	tc.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 || sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo) != pin {
			return fmt.Errorf("member server public key differs from configured pin")
		}
		return nil
	}
	transport := &http.Transport{
		TLSClientConfig:       tc,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		MaxConnsPerHost:       1,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	}
	httpClient := &http.Client{
		Transport:     transport,
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	rpc := connect.NewClient(
		connecthttp.NewTransport(
			memberHTTP{httpClient},
			cfg.URL,
			connecthttp.WithGRPC(),
			connecthttp.WithReadMaxBytes(4096),
			connecthttp.WithSendMaxBytes(4096),
			connecthttp.WithCompressors(),
		),
	)
	return memberconnect.NewMemberServiceClient(rpc), transport.CloseIdleConnections, nil
}

type identity struct {
	Member        string `json:"member"`
	Incarnation   string `json:"incarnation"`
	FailureDomain string `json:"failure_domain"`
	OwnerEpoch    int64  `json:"owner_epoch"`
}

func readIdentity(ctx context.Context, rpc memberconnect.MemberServiceClient, cfg config) (identity, error) {
	wire, err := rpc.Identity(ctx, &memberv1.IdentityRequest{})
	if err != nil {
		return identity{}, err
	}
	if wire.Release != buildRelease() || wire.Member != cfg.ID || wire.FailureDomain != cfg.Domain ||
		wire.OwnerEpoch < 1 {
		return identity{}, fmt.Errorf("member identity/release does not match fixture")
	}
	if _, err := replicaconfig.ParseID(wire.Incarnation); err != nil {
		return identity{}, err
	}
	return identity{
		Member:        wire.Member,
		Incarnation:   wire.Incarnation,
		FailureDomain: wire.FailureDomain,
		OwnerEpoch:    wire.OwnerEpoch,
	}, nil
}
