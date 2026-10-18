//go:build linux

package receiptstore

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/cotocisternas/fry/internal/telemetry"

	memberconnect "github.com/cotocisternas/fry/gen/member/go/fry/member/v1/memberv1connect"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
)

// MemberServer owns its member. The CA authenticates peers; exact coordinator
// SPKI pins authorize them on every HTTP/2 gRPC call, including pooled sessions.
type MemberServer struct {
	owner *memberOwner
	http  *http.Server
	tls   *tls.Config
	keys  map[[32]byte]bool
	slots chan struct{}
}
type MemberServerOptions struct {
	TLS             *tls.Config
	CoordinatorKeys [][32]byte
	MaxRequests     int
	Timeout         time.Duration
}
type MemberIdentity struct {
	Member        ID     `json:"member"`
	Incarnation   ID     `json:"incarnation"`
	FailureDomain string `json:"failure_domain"`
	OwnerEpoch    int64  `json:"owner_epoch"`
}

func NewMemberServer(ctx context.Context, m *LocalMember, opts MemberServerOptions) (*MemberServer, error) {
	if !validMemberServer(m, opts) {
		return nil, fmt.Errorf("receiptstore: invalid member server configuration")
	}

	tc := opts.TLS.Clone()
	tc.MinVersion = tls.VersionTLS13
	tc.ClientAuth = tls.RequireAndVerifyClientCert
	tc.ClientCAs = tc.ClientCAs.Clone()
	tc.NextProtos = []string{"h2"}
	o, err := ownMember(ctx, m)
	if err != nil {
		return nil, err
	}
	s := &MemberServer{owner: o, tls: tc, keys: map[[32]byte]bool{}, slots: make(chan struct{}, opts.MaxRequests)}
	for _, key := range opts.CoordinatorKeys {
		s.keys[key] = true
	}
	rpc := connect.NewServer(s.intercept)
	memberconnect.RegisterMemberServiceHandler(rpc, &memberRPC{server: s})
	mux := http.NewServeMux()
	connecthttp.Mount(
		mux,
		rpc,
		connecthttp.WithReadMaxBytes(memberFrameLimit),
		connecthttp.WithSendMaxBytes(memberFrameLimit),
		connecthttp.WithCompressors(),
	)
	s.http = &http.Server{
		TLSConfig:         tc,
		HTTP2:             memberHTTP2(opts.MaxRequests),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       opts.Timeout,
		WriteTimeout:      opts.Timeout,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	s.http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), opts.Timeout)
		defer cancel()
		// Request-scoped cancellation interrupts input and output without
		// imposing a deadline on unrelated HTTP/2 streams.
		ctx = context.WithValue(ctx, memberBodyKey{}, r.Body)
		ctx = context.WithValue(ctx, memberResponseKey{}, w)
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
	s.http.Handler = telemetry.FromContext(ctx).InternalHTTPHandler("member.grpc", s.http.Handler)
	servingCtx := context.WithoutCancel(ctx)
	s.http.BaseContext = func(net.Listener) context.Context { return servingCtx }
	return s, nil
}

func validMemberServer(m *LocalMember, opts MemberServerOptions) bool {
	if m == nil || m.opts.MaxMessageBytes > 256<<20 || !validMemberServerTLS(opts.TLS) {
		return false
	}
	return len(opts.CoordinatorKeys) > 0 && len(opts.CoordinatorKeys) <= 256 && opts.MaxRequests >= 1 &&
		opts.MaxRequests <= 128 &&
		opts.Timeout >= time.Second &&
		opts.Timeout <= 5*time.Minute
}

func validMemberServerTLS(tc *tls.Config) bool {
	return tc != nil && len(tc.Certificates) > 0 && tc.ClientCAs != nil && tc.GetConfigForClient == nil
}

func (s *MemberServer) Serve(listener net.Listener) error {
	bounded := &memberListener{
		Listener: listener,
		slots:    make(chan struct{}, cap(s.slots)+8),
		errorLog: s.http.ErrorLog,
	}
	return s.http.ServeTLS(bounded, "", "")
}

func (s *MemberServer) Close(ctx context.Context) error {
	s.owner.cancel()
	return errors.Join(s.http.Close(), s.owner.Close(ctx))
}

func (s *MemberServer) authorized(state *tls.ConnectionState) bool {
	if state == nil || len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return false
	}
	chain := state.VerifiedChains[0]
	return s.keys[sha256.Sum256(chain[0].RawSubjectPublicKeyInfo)] && validMemberCertificates(state)
}

func (s *MemberServer) intercept(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		info, ok := connecthttp.ServerInfoForContext(ctx)
		if !ok || !s.authorized(info.TLS()) {
			return connect.NewError(connect.CodePermissionDenied, "member peer denied")
		}
		call, _ := connect.CallInfoForServerContext(ctx)
		if !matchingMemberCall(info, call) {
			return connect.NewError(connect.CodeFailedPrecondition, "member release or protocol mismatch")
		}
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		default:
			return connect.NewError(connect.CodeResourceExhausted, "member busy")
		}
		work, done, err := s.owner.begin(ctx)
		if err != nil {
			return memberRPCError(err)
		}
		defer done()
		stop, err := s.interruptBody(work)
		if err != nil {
			return memberRPCError(err)
		}
		defer stop()

		return memberRPCError(next(work, spec, stream))
	}
}

func matchingMemberCall(info *connecthttp.ServerInfo, call *connect.CallInfo) bool {
	return info.RequestProto() == "HTTP/2.0" && call.Protocol == connect.ProtocolNameGRPC &&
		call.Codec == connect.CodecNameProto &&
		call.RequestHeader().Get("Fry-Release") == memberRelease()
}

type memberBodyKey struct{}

// Bound raw connections before TLS allocates handshake state. Idle keep-alive
// connections count toward this budget; excess connections close without work.
type memberListener struct {
	net.Listener
	slots    chan struct{}
	errorLog *log.Logger
}

func (l *memberListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &memberConnection{Conn: c, release: func() { <-l.slots }}, nil
		default:
			if err := c.Close(); err != nil {
				// This rejected connection owns no admission slot. Its close
				// failure cannot invalidate the listening socket or expose peer
				// details through the server's diagnostics.
				logger := l.errorLog
				if logger == nil {
					logger = log.Default()
				}
				logger.Print("receiptstore: rejected connection close failed")
			}
		}
	}
}

type memberConnection struct {
	net.Conn
	once    sync.Once
	release func()
	err     error
}

func (c *memberConnection) Close() error {
	c.once.Do(func() { c.err = c.Conn.Close(); c.release() })
	return c.err
}

type memberResponseKey struct{}

func (s *MemberServer) interruptBody(ctx context.Context) (func(), error) {
	body, ok := ctx.Value(memberBodyKey{}).(io.ReadCloser)
	if !ok {
		return nil, ErrConflict
	}
	output, ok := ctx.Value(memberResponseKey{}).(http.ResponseWriter)
	if !ok {
		return nil, ErrConflict
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(stopped)
		// Go's HTTP/2 ResponseController deadlines apply to this stream. Closing
		// input alone cannot interrupt a response blocked on peer flow control.
		err := errors.Join(body.Close(), http.NewResponseController(output).SetWriteDeadline(time.Now()))
		if err != nil {
			s.owner.cancel()
		}
	})
	return func() {
		if !stop() {
			<-stopped
		}
	}, nil
}
