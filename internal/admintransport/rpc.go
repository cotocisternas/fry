//go:build linux

package admintransport

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"

	"github.com/cotocisternas/fry/gen/admin/go/fry/admin/v1/adminv1connect"
	"github.com/cotocisternas/fry/internal/admin"
	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
)

func (h *HTTP) newRPC() http.Handler {
	server := connect.NewServer(h.rpcSecurity)
	adminv1connect.RegisterAdminServiceHandler(server, &rpcService{h: h})
	mux := http.NewServeMux()
	codec := connectproto.NewJSONCodec()
	codec.UnmarshalOptions.DiscardUnknown = false
	connecthttp.Mount(
		mux,
		server,
		connecthttp.WithReadMaxBytes(1<<20),
		connecthttp.WithSendMaxBytes(1<<20),
		connecthttp.WithCodecs(codec, strictBinaryCodec{connectproto.NewBinaryCodec()}),
	)
	return mux
}

func (h *HTTP) rpcToken(ctx context.Context) string {
	info, _ := connect.CallInfoForServerContext(ctx)
	request := &http.Request{Header: http.Header{"Cookie": info.RequestHeader().Values("Cookie")}}
	return h.token(request)
}

func (h *HTTP) rpcSecurity(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		if err := h.authorizeRPC(ctx, spec.Procedure); err != nil {
			return rpcError(err)
		}
		return next(ctx, spec, stream)
	}
}

func (h *HTTP) authorizeRPC(ctx context.Context, procedure string) error {
	info, _ := connect.CallInfoForServerContext(ctx)
	if info.RequestHeader().Get("Origin") != h.origin.String() {
		return admin.ErrForbidden
	}
	switch procedure {
	case adminv1connect.AdminServiceLoginProcedure,
		adminv1connect.AdminServiceGetConfigProcedure,
		adminv1connect.AdminServiceGetSessionProcedure:
		return nil
	}
	who, err := h.service.Identity(ctx, h.rpcToken(ctx))
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(info.RequestHeader().Get("X-FRY-CSRF")), []byte(who.CSRF)) != 1 {
		return admin.ErrForbidden
	}
	return nil
}

func rpcError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, "Request canceled; inspect state before repeating a mutation.")
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(
			connect.CodeDeadlineExceeded,
			"Request timed out; inspect state before repeating a mutation.",
		)
	case errors.Is(err, admin.ErrUnauthorized):
		return connect.NewError(connect.CodeUnauthenticated, "Sign in to continue.")
	case errors.Is(err, admin.ErrForbidden):
		return connect.NewError(connect.CodePermissionDenied, "You do not have permission for this action.")
	case errors.Is(err, admin.ErrInvalid), errors.Is(err, maildirectory.ErrInvalid):
		return connect.NewError(connect.CodeInvalidArgument, "Check the request fields.")
	case errors.Is(err, admin.ErrConflict),
		errors.Is(err, receiptstore.ErrSenderAuthorization),
		errors.Is(err, receiptstore.ErrConflict),
		errors.Is(err, maildirectory.ErrConflict):
		return connect.NewError(connect.CodeAlreadyExists, "This change conflicts with existing state.")
	case errors.Is(err, maildirectory.ErrNotFound),
		errors.Is(err, receiptstore.ErrNotFound),
		errors.Is(err, receiptstore.ErrExpired):
		return connect.NewError(connect.CodeNotFound, "The requested record is unavailable.")
	default:
		return connect.NewError(
			connect.CodeUnavailable,
			"Administration is temporarily unavailable; inspect state before repeating a mutation.",
		)
	}
}

// Mutations must not silently ignore fields supplied by a newer client.
type strictBinaryCodec struct{ *connectproto.BinaryCodec }

func (codec strictBinaryCodec) UnmarshalRead(ctx context.Context, source io.Reader, target any) error {
	if err := codec.BinaryCodec.UnmarshalRead(ctx, source, target); err != nil {
		return err
	}
	message, ok := target.(proto.Message)
	if !ok || unknownProtoFields(message.ProtoReflect()) {
		return admin.ErrInvalid
	}
	return nil
}

func unknownProtoFields(message protoreflect.Message) bool {
	if len(message.GetUnknown()) != 0 {
		return true
	}
	unknown := false
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Kind() != protoreflect.MessageKind {
			return true
		}
		if field.IsList() {
			unknown = unknownProtoList(value.List())
		} else {
			unknown = unknownProtoFields(value.Message())
		}
		return !unknown
	})
	return unknown
}

func unknownProtoList(values protoreflect.List) bool {
	for i := range values.Len() {
		if unknownProtoFields(values.Get(i).Message()) {
			return true
		}
	}
	return false
}
