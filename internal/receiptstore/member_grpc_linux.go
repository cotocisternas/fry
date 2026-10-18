//go:build linux

package receiptstore

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	memberv1 "github.com/cotocisternas/fry/gen/member/go/fry/member/v1"
	memberconnect "github.com/cotocisternas/fry/gen/member/go/fry/member/v1/memberv1connect"

	"connectrpc.com/connect/v2"
)

const (
	memberChunkSize  = 64 << 10
	memberFrameLimit = memberChunkSize + 4096
)

// release can be set with -ldflags=-X=github.com/cotocisternas/fry/internal/receiptstore.release=TAG.
// Source builds also bind the revision; test binaries share the devel identity.
var release string

func memberRelease() string {
	if release != "" {
		return release
	}
	parts := []string{"prototype-member-grpc-v1"}
	if info, ok := debug.ReadBuildInfo(); ok {
		parts = append(parts, info.Main.Version)
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				parts = append(parts, setting.Value)
			}
			if setting.Key == "vcs.modified" {
				parts = append(parts, "dirty="+setting.Value)
			}
		}
	}
	return strings.Join(parts, "/")
}

func validMemberCertificates(state *tls.ConnectionState) bool {
	if state == nil || len(state.VerifiedChains) == 0 {
		return false
	}
	now := time.Now()
	for _, certificate := range state.VerifiedChains[0] {
		if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
			return false
		}
	}
	return len(state.VerifiedChains[0]) > 0
}

func memberRPCError(err error) error {
	if err == nil {
		return nil
	}
	// Closure uncertainty outranks every joined transport/status error: callers
	// must preserve closure debt and cannot treat another replica as a fallback.
	if errors.Is(err, errReadClosureUncertain) {
		return connect.NewError(connect.CodeDataLoss, "member read closure uncertain").WithCause(err)
	}
	if _, ok := errors.AsType[*connect.Error](err); ok {
		return err
	}
	code := connect.CodeUnavailable
	switch {
	case errors.Is(err, context.Canceled):
		code = connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		code = connect.CodeDeadlineExceeded
	case errors.Is(err, ErrConflict):
		code = connect.CodeInvalidArgument
	case errors.Is(err, ErrStale):
		code = connect.CodeFailedPrecondition
	case errors.Is(err, ErrBusy):
		code = connect.CodeResourceExhausted
	}
	return connect.NewError(code, "member operation unavailable").WithCause(err)
}

func remoteRPCError(err error) error {
	if err == nil {
		return nil
	}
	if connect.CodeOf(err) == connect.CodeDataLoss {
		return errors.Join(errReadClosureUncertain, err)
	}
	return err
}

func identityProto(id MemberIdentity) *memberv1.IdentityResponse {
	return &memberv1.IdentityResponse{
		Member:        id.Member.String(),
		Incarnation:   id.Incarnation.String(),
		FailureDomain: id.FailureDomain,
		OwnerEpoch:    id.OwnerEpoch,
		Release:       memberRelease(),
	}
}

func identityFromProto(value *memberv1.IdentityResponse) (MemberIdentity, error) {
	var id MemberIdentity
	if value == nil || value.Release != memberRelease() {
		return id, ErrConflict
	}
	var err error
	id.Member, err = idFrom(value.Member)
	if err != nil {
		return id, err
	}
	id.Incarnation, err = idFrom(value.Incarnation)
	if err != nil {
		return id, err
	}
	id.FailureDomain, id.OwnerEpoch = value.FailureDomain, value.OwnerEpoch
	if id.Member == (ID{}) || id.Incarnation == (ID{}) || id.FailureDomain == "" || id.OwnerEpoch < 1 {
		return id, ErrConflict
	}
	return id, nil
}

func targetClaim(target *memberv1.Target) (claim, error) {
	var c claim
	if target == nil {
		return c, ErrConflict
	}
	var err error
	c.Organization, err = idFrom(target.Organization)
	if err != nil {
		return c, err
	}
	c.Receipt, err = idFrom(target.Receipt)
	if err != nil {
		return c, err
	}
	if target.Attempt != "" {
		c.Attempt, err = idFrom(target.Attempt)
	}
	c.Epoch, c.Profile = target.Epoch, target.Profile
	if err != nil || c.Organization == (ID{}) || c.Receipt == (ID{}) {
		return c, ErrConflict
	}
	return c, nil
}

type memberRPC struct{ server *MemberServer }

func (rpc *memberRPC) Identity(ctx context.Context, _ *memberv1.IdentityRequest) (*memberv1.IdentityResponse, error) {
	s := rpc.server
	m := s.owner.member
	m.gate.RLock()
	defer m.gate.RUnlock()
	if m.closed || m.isFrozen() {
		return nil, ErrStale
	}
	return identityProto(
		MemberIdentity{Member: m.id, Incarnation: m.bytes.FileID(), FailureDomain: m.domain, OwnerEpoch: s.owner.epoch},
	), nil
}

func (rpc *memberRPC) checkTarget(target *memberv1.Target, incarnation bool) error {
	if target == nil || target.OwnerEpoch != rpc.server.owner.epoch {
		return ErrStale
	}
	id, err := idFrom(target.Incarnation)
	if err != nil || id == (ID{}) {
		return ErrConflict
	}
	if incarnation {
		m := rpc.server.owner.member
		m.gate.RLock()
		current := m.bytes.FileID()
		m.gate.RUnlock()
		if id != current {
			return ErrStale
		}
	}
	return nil
}

func (rpc *memberRPC) Write(
	ctx context.Context,
	stream memberconnect.MemberServiceWriteServerStream,
) (*memberv1.WriteResponse, error) {
	first, err := stream.Receive()
	if err != nil {
		return nil, err
	}
	target := first.GetTarget()
	if err = rpc.checkTarget(target, true); err != nil {
		return nil, err
	}
	c, err := targetClaim(target)
	if err != nil || c.Attempt == (ID{}) || c.Epoch < 1 || c.Profile < 1 {
		return nil, ErrConflict
	}
	body, ok := ctx.Value(memberBodyKey{}).(io.ReadCloser)
	if !ok {
		return nil, ErrConflict
	}
	source := newMemberChunkReader(func() ([]byte, *memberv1.CompressedChunk, *memberv1.Completion, error) {
		next, err := stream.Receive()
		return next.GetData(), next.GetCompressedData(), next.GetCompletion(), memberReceiveError(err)
	}, rpc.server.owner.member.opts.MaxMessageBytes, body)
	m := rpc.server.owner.member
	if err = m.writeOwned(ctx, &authority{pool: m.pool}, c, source, rpc.server.owner); err != nil {
		return nil, err
	}
	return &memberv1.WriteResponse{}, nil
}

func (rpc *memberRPC) Read(
	ctx context.Context,
	req *memberv1.ReadRequest,
	stream memberconnect.MemberServiceReadServerStream,
) error {
	if err := rpc.checkTarget(req.Target, false); err != nil {
		return err
	}
	c, err := targetClaim(req.Target)
	if err != nil {
		return err
	}
	operation, err := idFrom(req.Operation)
	if err != nil {
		return err
	}
	pin, err := idFrom(req.ParentPin)
	if err != nil || req.Length < 0 || req.Length > rpc.server.owner.member.opts.MaxMessageBytes {
		return ErrConflict
	}
	output := newMemberChunkWriter(
		func(data []byte, compressed *memberv1.CompressedChunk, end *memberv1.Completion) error {
			return stream.Send(memberReadFrame(data, compressed, end))
		},
		req.Length,
	)
	err = rpc.server.owner.read(ctx, operation, pin, c.Organization, c.Receipt, ID{}, req.Offset, req.Length, output)
	if err != nil {
		return err
	}
	return output.finish()
}

func (rpc *memberRPC) Cancel(ctx context.Context, req *memberv1.CancelRequest) (*memberv1.CancelResponse, error) {
	if err := rpc.checkTarget(req.Target, true); err != nil {
		return nil, err
	}
	c, err := targetClaim(req.Target)
	if err != nil || c.Attempt == (ID{}) {
		return nil, ErrConflict
	}
	m := rpc.server.owner.member
	exists, err := dbgen.New(m.pool).
		MemberServerServeCheckAttempt(ctx, dbgen.MemberServerServeCheckAttemptParams{
			ID:           c.Attempt.String(),
			Organization: c.Organization.String(),
			ReceiptID:    c.Receipt.String(),
		})
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	if err = m.cancelOwned(ctx, &authority{pool: m.pool}, c.Attempt, rpc.server.owner); err != nil {
		return nil, err
	}
	return &memberv1.CancelResponse{}, nil
}

// Flow-control windows bound unread HTTP/2 bytes independently of decoded
// protobuf chunks. Active storage calls have the separate server admission gate.
func memberHTTP2(maxStreams int) *http.HTTP2Config {
	return &http.HTTP2Config{
		MaxConcurrentStreams:          maxStreams,
		StrictMaxConcurrentRequests:   true,
		MaxDecoderHeaderTableSize:     4096,
		MaxEncoderHeaderTableSize:     4096,
		MaxReadFrameSize:              memberChunkSize,
		MaxReceiveBufferPerConnection: 1 << 20,
		MaxReceiveBufferPerStream:     2 * memberChunkSize,
	}
}
