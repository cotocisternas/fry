//go:build linux

package receiptstore

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"
	"github.com/cotocisternas/fry/internal/telemetry"

	memberv1 "github.com/cotocisternas/fry/gen/member/go/fry/member/v1"
	memberconnect "github.com/cotocisternas/fry/gen/member/go/fry/member/v1/memberv1connect"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
)

type RemoteMember struct {
	refreshMu  sync.Mutex
	mu         sync.RWMutex
	descriptor MemberIdentity
	endpoint   string
	client     *connect.Client
	transport  *http.Transport
	maxBytes   int64
	timeout    time.Duration
}
type RemoteMemberOptions struct {
	URL             string
	TLS             *tls.Config
	Member          ID
	FailureDomain   string
	ServerKey       [32]byte
	MaxMessageBytes int64
	MaxConnections  int
	Timeout         time.Duration
}

func validRemoteEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && (u.Path == "" || u.Path == "/") &&
		u.RawQuery == "" &&
		u.Fragment == "" &&
		u.Opaque == ""
}

func validRemoteTLS(tc *tls.Config) bool {
	return tc != nil && !tc.InsecureSkipVerify && tc.RootCAs != nil && len(tc.Certificates) > 0
}

func validRemoteOptions(opts RemoteMemberOptions) bool {
	return validRemoteEndpoint(opts.URL) && validRemoteTLS(opts.TLS) && opts.Member != (ID{}) &&
		opts.FailureDomain != "" &&
		opts.ServerKey != ([32]byte{}) &&
		validRemoteLimits(opts)
}

func validRemoteLimits(opts RemoteMemberOptions) bool {
	return opts.MaxMessageBytes >= 1 && opts.MaxMessageBytes <= 256<<20 && opts.MaxConnections >= 1 &&
		opts.MaxConnections <= 128 &&
		opts.Timeout >= time.Second &&
		opts.Timeout <= 5*time.Minute
}

func memberClientTLS(opts RemoteMemberOptions) *tls.Config {
	tc := opts.TLS.Clone()
	tc.RootCAs = tc.RootCAs.Clone()
	tc.MinVersion = tls.VersionTLS13
	tc.NextProtos = []string{"h2"}
	previous := tc.VerifyConnection
	tc.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 ||
			sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo) != opts.ServerKey {
			return ErrStale
		}
		if previous != nil {
			return previous(state)
		}
		return nil
	}
	return tc
}

func DialRemoteMember(ctx context.Context, opts RemoteMemberOptions) (*RemoteMember, error) {
	if !validRemoteOptions(opts) {
		return nil, fmt.Errorf("receiptstore: invalid remote member configuration")
	}
	tr := newMemberTransport(opts)
	m := &RemoteMember{transport: tr, maxBytes: opts.MaxMessageBytes, timeout: opts.Timeout}
	client := &http.Client{
		Transport:     telemetry.FromContext(ctx).HTTPTransport("member.rpc", tr),
		Timeout:       opts.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	m.client = connect.NewClient(
		connecthttp.NewTransport(
			memberHTTPClient{client},
			opts.URL,
			connecthttp.WithGRPC(),
			connecthttp.WithReadMaxBytes(memberFrameLimit),
			connecthttp.WithSendMaxBytes(memberFrameLimit),
			connecthttp.WithCompressors(),
		),
	)
	m.endpoint = opts.URL
	value, err := m.rpc().Identity(ctx, &memberv1.IdentityRequest{})
	if err == nil {
		m.descriptor, err = identityFromProto(value)
	}
	if err != nil {
		m.Close()
		return nil, err
	}
	if m.descriptor.Member != opts.Member || m.descriptor.FailureDomain != opts.FailureDomain {
		m.Close()
		return nil, ErrConflict
	}
	return m, nil
}

// HTTP metadata enforces release matching and validates the response. The
// connection write guard enforces certificate validity before transmission.
type memberHTTPClient struct{ client *http.Client }

func (c memberHTTPClient) Do(request *http.Request) (*http.Response, error) {
	request.Header.Set("Fry-Release", memberRelease())
	// G704: validated operator HTTPS endpoint, fixed generated procedures, pinned mTLS peer, no proxies or
	// redirects. Owner: FRY maintainers; expires: 2027-01-18.
	//nolint:gosec // See the rationale above.
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.ProtoMajor != 2 || !validMemberCertificates(response.TLS) {
		return nil, errors.Join(ErrStale, response.Body.Close())
	}
	return response, nil
}

func (m *RemoteMember) Close() { m.transport.CloseIdleConnections() }
func (m *RemoteMember) identity() MemberIdentity {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.descriptor
}
func (m *RemoteMember) available() bool { return true }
func (m *RemoteMember) target(c claim) *memberv1.Target {
	id := m.identity()
	return &memberv1.Target{
		Attempt:      c.Attempt.String(),
		Receipt:      c.Receipt.String(),
		Organization: c.Organization.String(),
		Epoch:        c.Epoch,
		Profile:      c.Profile,
		Incarnation:  id.Incarnation.String(),
		OwnerEpoch:   id.OwnerEpoch,
	}
}

func (m *RemoteMember) write(ctx context.Context, _ *authority, c claim, source io.ReadCloser) (resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	owned := &ownedStream{ReadCloser: source}
	defer func() { resultErr = errors.Join(resultErr, owned.Close()) }()
	spec := connect.Spec{
		Procedure:  memberconnect.MemberServiceWriteProcedure,
		StreamType: connect.StreamTypeClient,
		Schema: memberv1.File_fry_member_v1_member_proto.Services().ByName("MemberService").Methods().
			ByName("Write"),
	}
	stream, err := m.client.CallClientStream(ctx, spec)
	if err != nil {
		owned.cancelRead()
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, stream.Close()) }()
	stop := context.AfterFunc(ctx, owned.cancelRead)
	defer stop()
	if err = stream.Send(
		&memberv1.WriteRequest{Frame: &memberv1.WriteRequest_Target{Target: m.target(c)}},
	); err != nil {
		owned.cancelRead()
		return err
	}
	done := make(chan error, 1)
	go func() { done <- m.sendOriginal(stream, owned) }()
	err = stream.Receive(&memberv1.WriteResponse{})
	if err == nil {
		if end := memberReceiveError(
			stream.Receive(&memberv1.WriteResponse{}),
		); end != io.EOF { //nolint:errorlint // Integrity requires an unadorned EOF sentinel.
			err = errors.Join(ErrConflict, end)
		}
	}
	// A terminal response, including a premature success from a faulty peer,
	// cannot leave the original producer blocked. Success still requires the
	// producer's own exact EOF and completion frame.
	owned.cancelRead()
	if err != nil {
		cancel()
	}
	return errors.Join(remoteRPCError(err), <-done)
}

func (m *RemoteMember) sendOriginal(stream connect.ClientStream, source io.Reader) error {
	output := newMemberChunkWriter(
		func(data []byte, compressed *memberv1.CompressedChunk, end *memberv1.Completion) error {
			return stream.Send(memberWriteFrame(data, compressed, end))
		},
		m.maxBytes,
	)
	_, err := io.CopyBuffer(output, source, make([]byte, memberChunkSize))
	if err == nil {
		err = output.finish()
	}
	return errors.Join(err, stream.CloseSend())
}

func (m *RemoteMember) cancelAttempt(ctx context.Context, a *authority, attempt ID) error {
	row, err := dbgen.New(a.pool).RemoteMemberCancelAttempt(ctx, attempt.String())
	if err != nil {
		return err
	}
	org, err := idFrom(row.Organization)
	if err != nil {
		return err
	}
	receipt, err := idFrom(row.ReceiptID)
	if err != nil {
		return err
	}
	_, err = m.rpc().
		Cancel(ctx, &memberv1.CancelRequest{
			Target: m.target(claim{Attempt: attempt, Organization: org, Receipt: receipt}),
		})
	return remoteRPCError(err)
}

func (m *RemoteMember) read(
	ctx context.Context,
	pin, org, receipt ID,
	_ hotstore.Locator,
	offset, length int64,
	dst io.Writer,
) (resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	operation, err := hotstore.NewID()
	if err != nil {
		return err
	}
	stream, err := m.rpc().
		Read(ctx, &memberv1.ReadRequest{
			Target:    m.target(claim{Organization: org, Receipt: receipt}),
			Operation: operation.String(),
			ParentPin: pin.String(),
			Offset:    offset,
			Length:    length,
		})
	if err != nil {
		return remoteRPCError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, stream.Close()) }()
	input := newMemberChunkReader(func() ([]byte, *memberv1.CompressedChunk, *memberv1.Completion, error) {
		frame, err := stream.Receive()
		return frame.GetData(), frame.GetCompressedData(), frame.GetCompletion(), memberReceiveError(err)
	}, length, nil)
	input.exact = true
	n, err := io.Copy(dst, input)
	if err == nil && n != length {
		err = io.ErrUnexpectedEOF
	}
	return remoteRPCError(err)
}

func (m *RemoteMember) adopt(current MemberIdentity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.descriptor
	if current.Member != old.Member || current.FailureDomain != old.FailureDomain ||
		current.OwnerEpoch != old.OwnerEpoch ||
		current.Incarnation == (ID{}) {
		return ErrStale
	}
	m.descriptor = current
	return nil
}

func (m *RemoteMember) refresh(ctx context.Context) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	wire, err := m.rpc().Identity(ctx, &memberv1.IdentityRequest{})
	if err != nil {
		return remoteRPCError(err)
	}
	current, err := identityFromProto(wire)
	if err != nil {
		return err
	}
	return m.adopt(current)
}

func (m *RemoteMember) rpc() memberconnect.MemberServiceClient {
	return memberconnect.NewMemberServiceClient(m.client)
}
