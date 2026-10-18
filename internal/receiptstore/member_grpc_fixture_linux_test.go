//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	memberv1 "github.com/cotocisternas/fry/gen/member/go/fry/member/v1"
	memberconnect "github.com/cotocisternas/fry/gen/member/go/fry/member/v1/memberv1connect"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
)

// This fixture retains the existing storage tests' scenario vocabulary, while
// every network exchange uses generated protobuf RPCs over HTTP/2 mutual TLS.
type memberRequest struct {
	Attempt, Receipt, Organization, Incarnation ID
	OwnerEpoch, Epoch, Profile                  int64
	Operation, ParentPin                        ID
	Offset, Length                              int64
}
type automaticRotationResponse struct {
	State    string          `json:"state"`
	Identity *MemberIdentity `json:"identity,omitempty"`
}

func legacyTarget(r memberRequest) *memberv1.Target {
	return &memberv1.Target{
		Attempt:      r.Attempt.String(),
		Receipt:      r.Receipt.String(),
		Organization: r.Organization.String(),
		Incarnation:  r.Incarnation.String(),
		OwnerEpoch:   r.OwnerEpoch,
		Epoch:        r.Epoch,
		Profile:      r.Profile,
	}
}

func legacyReplicaCall(t *testing.T, client *http.Client, raw string, metadata memberRequest) (*http.Response, error) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	ctx, info := connect.NewClientContext(t.Context())
	info.RequestHeader().Set("Fry-Test-Route", u.Path)
	clientRPC := connect.NewClient(
		connecthttp.NewTransport(
			memberHTTPClient{client},
			"https://"+u.Host,
			connecthttp.WithGRPC(),
			connecthttp.WithReadMaxBytes(memberFrameLimit),
			connecthttp.WithSendMaxBytes(memberFrameLimit),
		),
	)
	rpc := memberconnect.NewMemberServiceClient(clientRPC)
	value, err := legacyReplicaOperation(ctx, rpc, u.Path, metadata)
	if err != nil {
		status := http.StatusServiceUnavailable
		if connect.CodeOf(err) == connect.CodePermissionDenied {
			status = http.StatusForbidden
		}
		if connect.CodeOf(err) == connect.CodeFailedPrecondition || connect.CodeOf(err) == connect.CodeInvalidArgument {
			status = http.StatusConflict
		}
		if !strings.HasSuffix(u.Path, "/status") && u.Path != "/v1/rotate-if-needed" &&
			connect.CodeOf(err) == connect.CodeInvalidArgument {
			status = http.StatusServiceUnavailable
		}

		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader("{}")),
			Header:     make(http.Header),
		}, nil
	}
	var data bytes.Buffer
	if err = json.NewEncoder(&data).Encode(value); err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(&data),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func legacyReplicaOperation(
	ctx context.Context,
	rpc memberconnect.MemberServiceClient,
	path string,
	r memberRequest,
) (any, error) {
	switch path {
	case "/v1/identity":
		wire, err := rpc.Identity(ctx, &memberv1.IdentityRequest{})
		if err != nil {
			return nil, err
		}
		return identityFromProto(wire)
	case "/v1/cancel":
		return rpc.Cancel(ctx, &memberv1.CancelRequest{Target: legacyTarget(r)})
	default:
		return legacyReplicaMaintenance(ctx, rpc, path, r)
	}
}

func legacyReplicaMaintenance(
	ctx context.Context,
	rpc memberconnect.MemberServiceClient,
	path string,
	r memberRequest,
) (any, error) {
	kinds := map[string]memberv1.MaintenanceKind{
		"/v1/reclaim":          memberv1.MaintenanceKind_MAINTENANCE_KIND_REWRITE,
		"/v1/rotate":           memberv1.MaintenanceKind_MAINTENANCE_KIND_ROTATE,
		"/v1/reclaim-sealed":   memberv1.MaintenanceKind_MAINTENANCE_KIND_SEALED_RECLAIM,
		"/v1/rotate-if-needed": memberv1.MaintenanceKind_MAINTENANCE_KIND_AUTO_ROTATE,
	}
	result, err := rpc.Maintenance(
		ctx,
		&memberv1.MaintenanceRequest{
			Target:     legacyTarget(r),
			Operation:  r.Operation.String(),
			Kind:       kinds[strings.TrimSuffix(path, "/status")],
			StatusOnly: strings.HasSuffix(path, "/status"),
		},
	)
	if err != nil {
		return nil, err
	}
	legacy := automaticRotationResponse{State: result.State}
	if result.Identity != nil {
		id, err := identityFromProto(result.Identity)
		if err != nil {
			return nil, err
		}
		legacy.Identity = &id
	}
	return legacy, nil
}

func decodeMemberJSON(response *http.Response, dst any) error {
	return json.NewDecoder(response.Body).Decode(dst)
}

type legacyRouteKey struct{}

func legacyReplicaWrapper(wrap func(http.Handler) http.Handler, next http.Handler) http.Handler {
	wrapped := wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clone := r.Clone(r.Context())
		original, ok := r.Context().Value(legacyRouteKey{}).(string)
		if !ok {
			panic("missing fixture route")
		}
		clone.URL.Path = original
		next.ServeHTTP(w, clone)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		original := r.URL.Path
		clone := r.Clone(context.WithValue(r.Context(), legacyRouteKey{}, original))
		legacy := r.Header.Get("Fry-Test-Route")
		if legacy == "" {
			legacy = legacyProcedurePath(original)
		}
		clone.URL.Path = legacy
		wrapped.ServeHTTP(w, clone)
	})
}

func legacyProcedurePath(procedure string) string {
	paths := map[string]string{
		memberconnect.MemberServiceIdentityProcedure:      "/v1/identity",
		memberconnect.MemberServiceWriteProcedure:         "/v1/write",
		memberconnect.MemberServiceReadProcedure:          "/v1/read",
		memberconnect.MemberServiceCancelProcedure:        "/v1/cancel",
		memberconnect.MemberServiceMaintenanceProcedure:   "/v1/reclaim",
		memberconnect.MemberServiceStorageStatusProcedure: "/v1/storage-status",
	}
	return paths[procedure]
}
