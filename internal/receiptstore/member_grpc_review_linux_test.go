//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"

	memberv1 "github.com/cotocisternas/fry/gen/member/go/fry/member/v1"
	memberconnect "github.com/cotocisternas/fry/gen/member/go/fry/member/v1/memberv1connect"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
)

func memberClosureErrors() map[string]error {
	return map[string]error{
		"connect":  connect.NewError(connect.CodeUnavailable, "send failed"),
		"cancel":   context.Canceled,
		"deadline": context.DeadlineExceeded,
		"conflict": ErrConflict,
		"stale":    ErrStale,
		"busy":     ErrBusy,
	}
}

func TestMemberGRPCClosureUncertaintyDominatesJoinedError(t *testing.T) {
	for name, failure := range memberClosureErrors() {
		t.Run(name, func(t *testing.T) {
			wire := memberRPCError(errors.Join(failure, errReadClosureUncertain))
			requireReceiptCondition(
				t,
				connect.CodeOf(wire) == connect.CodeDataLoss,
				"closure uncertainty lost its DATA_LOSS marker",
				wire,
			)
			requireReceiptCondition(
				t,
				errors.Is(remoteRPCError(wire), errReadClosureUncertain),
				"remote closure marker lost",
				wire,
			)
		})
	}
}

func memberReviewServer(
	t *testing.T,
	certs replicaTLS,
	rpc *connect.Server,
	wrap func(http.Handler) http.Handler,
) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	connecthttp.Mount(
		mux,
		rpc,
		connecthttp.WithReadMaxBytes(memberFrameLimit),
		connecthttp.WithSendMaxBytes(memberFrameLimit),
	)
	server := httptest.NewUnstartedServer(wrap(mux))
	server.EnableHTTP2 = true
	server.TLS = certs.server.Clone()
	server.TLS.MinVersion = tls.VersionTLS13
	server.TLS.ClientAuth = tls.RequireAndVerifyClientCert
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func TestMemberGRPCJoinedClosureUncertaintySurvivesWire(t *testing.T) {
	certs := replicaCertificates(t)
	rpc := connect.NewServer()
	failures := memberClosureErrors()
	rpc.Register(
		connect.Method{
			Spec: connect.Spec{
				Procedure:  memberconnect.MemberServiceReadProcedure,
				StreamType: connect.StreamTypeServer,
			},
			Handler: func(_ context.Context, _ connect.Spec, stream connect.ServerStream) error {
				req := &memberv1.ReadRequest{}
				if err := stream.Receive(req); err != nil {
					return err
				}
				return memberRPCError(errors.Join(failures[req.Target.Attempt], errReadClosureUncertain))
			},
		},
	)
	server := memberReviewServer(t, certs, rpc, func(next http.Handler) http.Handler { return next })
	client := memberTestRPC(certs.allowed, server.URL, memberRelease())
	for name := range failures {
		t.Run(name, func(t *testing.T) {
			stream, err := client.Read(t.Context(), &memberv1.ReadRequest{Target: &memberv1.Target{Attempt: name}})
			requireReceiptNoError(t, err)
			defer func() { requireReceiptNoError(t, stream.Close()) }()
			_, err = stream.Receive()
			requireReceiptCondition(t, connect.CodeOf(err) == connect.CodeDataLoss, "wire lost DATA_LOSS", err)
			requireReceiptCondition(
				t,
				errors.Is(remoteRPCError(err), errReadClosureUncertain),
				"wire lost closure marker",
				err,
			)
		})
	}
}

type memberReviewRequests struct {
	calls, bytes atomic.Int64
}

type memberReviewBody struct {
	io.ReadCloser
	bytes *atomic.Int64
}

func (b memberReviewBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.bytes.Add(int64(n))
	return n, err
}

func (r *memberReviewRequests) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.calls.Add(1)
		req.Body = memberReviewBody{ReadCloser: req.Body, bytes: &r.bytes}
		next.ServeHTTP(w, req)
	})
}

func memberReviewIdentityRPC(identity MemberIdentity) *connect.Server {
	rpc := connect.NewServer()
	rpc.Register(
		connect.Method{
			Spec: connect.Spec{
				Procedure:  memberconnect.MemberServiceIdentityProcedure,
				StreamType: connect.StreamTypeUnary,
			},
			Handler: func(_ context.Context, _ connect.Spec, stream connect.ServerStream) error {
				if err := stream.Receive(&memberv1.IdentityRequest{}); err != nil {
					return err
				}
				return stream.Send(identityProto(identity))
			},
		},
	)
	rpc.Register(
		connect.Method{
			Spec: connect.Spec{
				Procedure:  memberconnect.MemberServiceWriteProcedure,
				StreamType: connect.StreamTypeClient,
			},
			Handler: func(_ context.Context, _ connect.Spec, stream connect.ServerStream) error {
				for {
					err := memberReceiveError(stream.Receive(&memberv1.WriteRequest{}))
					//nolint:errorlint // The test peer accepts only normalized clean EOF.
					if err == io.EOF {
						return stream.Send(&memberv1.WriteResponse{})
					}
					if err != nil {
						return err
					}
				}
			},
		},
	)
	return rpc
}

func TestMemberGRPCExpiredPooledTLSNeverDispatchesWrite(t *testing.T) {
	// Certificate times encode whole seconds. Leave enough initial lifetime for
	// both successful calls, then cross the signed certificate's actual expiry.
	expiry := time.Now().Truncate(time.Second).Add(3 * time.Second)
	certs := replicaCertificatesUntil(t, expiry)
	identity := MemberIdentity{Member: newID(t), Incarnation: newID(t), FailureDomain: "review", OwnerEpoch: 1}
	requests := &memberReviewRequests{}
	server := memberReviewServer(t, certs, memberReviewIdentityRPC(identity), requests.wrap)
	remote, err := DialRemoteMember(
		t.Context(),
		RemoteMemberOptions{
			URL:             server.URL,
			TLS:             receiptTestValue[*http.Transport](t, certs.allowed.Transport).TLSClientConfig,
			Member:          identity.Member,
			FailureDomain:   identity.FailureDomain,
			ServerKey:       sha256.Sum256(certs.server.Certificates[0].Leaf.RawSubjectPublicKeyInfo),
			MaxMessageBytes: 1 << 20,
			MaxConnections:  1,
			Timeout:         5 * time.Second,
		},
	)
	requireReceiptNoError(t, err)
	defer remote.Close()
	var reused atomic.Bool
	ctx := httptrace.WithClientTrace(
		t.Context(),
		&httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused.Store(info.Reused) }},
	)
	_, err = remote.rpc().Identity(ctx, &memberv1.IdentityRequest{})
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, reused.Load() && requests.calls.Load() == 2, "fixture did not reuse its TLS connection")
	before := requests.bytes.Load()
	timer := time.NewTimer(time.Until(expiry) + 20*time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	requireExpiredReplicaRejectsWrite(t, remote, requests, before)
}

func TestMemberGRPCJoinedClosureDebtPreventsReplicaFallback(t *testing.T) {
	failures := memberClosureErrors()
	for _, name := range []string{"connect", "cancel", "conflict"} {
		t.Run(name, func(t *testing.T) {
			memberReviewClosureDebt(t, failures[name])
		})
	}
}

func memberReviewClosureDebt(t *testing.T, failure error) {
	t.Helper()
	f := sealedReadFixture(t)
	members, _ := remoteFixture(t, f)
	store, err := NewWithMembers(t.Context(), f.pool, [2]Member{members[0], members[1]}, f.s.db.opts)
	requireReceiptNoError(t, err)
	receipt := accept(t, store, newID(t), f.envelope(t), string(sealedReadBody()))
	for _, member := range f.members {
		requireReceiptNoError(t, store.RotateHot(t.Context(), member.id))
	}
	first, other := f.members[0], f.members[1]
	if other.id.String() < first.id.String() {
		first, other = other, first
	}
	reallyClosed, fallbackCloses := injectSealedDescriptorClosureLoss(t, first, other)
	closeImage := first.imageClose
	first.imageClose = func(image *hotstore.Image) error { return errors.Join(closeImage(image), failure) }
	defer first.readClosureUncertain.Store(false)
	// A zero-length read has delivered no payload, so only the uncertainty marker
	// prevents replica fallback and parent-pin cleanup after the joined failure.
	var output bytes.Buffer
	err = store.Read(t.Context(), f.org, receipt.ID, 0, 0, &output)
	requireReceiptCondition(t, errors.Is(err, errReadClosureUncertain), "joined failure lost closure uncertainty", err)
	requireReceiptCondition(t, reallyClosed.Load(), "fixture did not close the real image descriptor")
	requireSealedClosureDebt(t, f, first, true, receipt, fallbackCloses)
}

func requireExpiredReplicaRejectsWrite(
	t *testing.T,
	remote *RemoteMember,
	requests *memberReviewRequests,
	before int64,
) {
	t.Helper()
	err := remote.write(
		t.Context(),
		nil,
		claim{},
		io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("private original"), 4096))),
	)
	requireReceiptCondition(t, err != nil, "expired peer accepted write")
	requireReceiptCondition(
		t,
		requests.calls.Load() == 2,
		"expired connection dispatched a request",
		requests.calls.Load(),
	)
	requireReceiptCondition(
		t,
		requests.bytes.Load() == before,
		"expired connection transmitted request body",
		requests.bytes.Load()-before,
	)
	t.Logf(
		"verified pooled TLS: calls before/after expiry=2/%d, request body bytes "+
			"before/after=%d/%d",
		requests.calls.Load(),
		before,
		requests.bytes.Load(),
	)
}
