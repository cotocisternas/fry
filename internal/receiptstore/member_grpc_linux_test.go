//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	memberv1 "github.com/cotocisternas/fry/gen/member/go/fry/member/v1"
	memberconnect "github.com/cotocisternas/fry/gen/member/go/fry/member/v1/memberv1connect"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
)

type memberTestHTTPClient struct {
	client  *http.Client
	release string
}

func (c memberTestHTTPClient) Do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Fry-Release", c.release)
	// Local TLS test fixture endpoint and generated RPC paths only.
	// Owner: FRY maintainers; expires: 2027-01-18.
	//nolint:gosec // G704: local test fixture endpoint.
	return c.client.Do(req)
}

func memberTestRPC(client *http.Client, url, release string) memberconnect.MemberServiceClient {
	transport := connecthttp.NewTransport(
		memberTestHTTPClient{client, release},
		url,
		connecthttp.WithGRPC(),
		connecthttp.WithReadMaxBytes(memberFrameLimit),
	)
	return memberconnect.NewMemberServiceClient(connect.NewClient(transport))
}

func memberTestClaim(t *testing.T, f *fixture, server *MemberServer) claim {
	t.Helper()
	e, data, err := normalize(f.envelope(t))
	requireReceiptNoError(t, err)
	identity := f.members[0].identity()
	identity.OwnerEpoch = server.owner.epoch
	c, replay, err := f.s.db.prepareMembers(
		t.Context(),
		newID(t),
		e,
		data,
		[2]MemberIdentity{identity, f.members[1].identity()},
	)
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, replay == nil, "fresh attempt replayed")
	execSQL(
		t,
		f.pool,
		`UPDATE fry_target SET owner_epoch=$1 WHERE attempt=$2 AND member=$3`,
		server.owner.epoch,
		c.Attempt.String(),
		f.members[0].id.String(),
	)
	return c
}

func memberTestWrite(t *testing.T, rpc memberconnect.MemberServiceClient, target *memberv1.Target, body string) error {
	t.Helper()
	stream, err := rpc.Write(t.Context())
	if err != nil {
		return err
	}
	if err = stream.Send(&memberv1.WriteRequest{Frame: &memberv1.WriteRequest_Target{Target: target}}); err != nil {
		return err
	}
	if err = stream.Send(&memberv1.WriteRequest{Frame: &memberv1.WriteRequest_Data{Data: []byte(body)}}); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(body))
	if err = stream.Send(
		&memberv1.WriteRequest{
			Frame: &memberv1.WriteRequest_Completion{
				Completion: &memberv1.Completion{Size: int64(len(body)), Sha256: sum[:]},
			},
		},
	); err != nil {
		return err
	}
	_, err = stream.CloseAndReceive()
	return err
}

func TestMemberGRPCIdentityRequiresPinnedCoordinatorAndMatchingRelease(t *testing.T) {
	f := setup(t)
	var h2 atomic.Bool
	wrapper := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h2.Store(r.ProtoMajor == 2 && r.Header.Get("Content-Type") == "application/grpc")
			next.ServeHTTP(w, r)
		})
	}
	server, certs, url := memberHTTP(t, f, wrapper)
	good := memberTestRPC(certs.allowed, url, memberRelease())
	wire, err := good.Identity(t.Context(), &memberv1.IdentityRequest{})
	requireReceiptNoError(t, err)
	identity, err := identityFromProto(wire)
	requireReceiptNoError(t, err)
	requireReceiptCondition(
		t,
		identity.Member == f.members[0].id && identity.OwnerEpoch == server.owner.epoch && h2.Load(),
		"identity did not use HTTP/2 gRPC",
	)
	denied := memberTestRPC(certs.other, url, memberRelease())
	_, err = denied.Identity(t.Context(), &memberv1.IdentityRequest{})
	requireReceiptCondition(
		t,
		connect.CodeOf(err) == connect.CodePermissionDenied,
		"CA membership became authorization",
		err,
	)
	mismatch := memberTestRPC(certs.allowed, url, memberRelease()+"-other")
	_, err = mismatch.Identity(t.Context(), &memberv1.IdentityRequest{})
	requireReceiptCondition(t, connect.CodeOf(err) == connect.CodeFailedPrecondition, "release mismatch admitted", err)
}

func TestMemberGRPCWriteRechecksAuthorityAndReadCompletesAfterClosure(t *testing.T) {
	f := setup(t)
	server, certs, url := memberHTTP(t, f)
	rpc := memberTestRPC(certs.allowed, url, memberRelease())
	c := memberTestClaim(t, f, server)
	target := legacyTarget(replicaMetadata(c, server))
	target.OwnerEpoch++
	requireReceiptCondition(t, memberTestWrite(t, rpc, target, "payload") != nil, "stale owner admitted")
	target.OwnerEpoch--
	target.Organization = newID(t).String()
	requireReceiptCondition(t, memberTestWrite(t, rpc, target, "payload") != nil, "forged organization admitted")
	target.Organization = c.Organization.String()
	requireReceiptNoError(t, memberTestWrite(t, rpc, target, "original bytes"))
	requireReceiptNoError(
		t,
		f.members[1].write(t.Context(), f.s.db, c, io.NopCloser(strings.NewReader("original bytes"))),
	)
	receipt, err := f.s.db.finalize(t.Context(), c)
	requireReceiptNoError(t, err)
	parent, err := f.s.Open(t.Context(), f.org, receipt.ID, 0, receipt.Size)
	requireReceiptNoError(t, err)
	defer func() { requireReceiptNoError(t, parent.Close()) }()
	var parentID string
	err = f.pool.QueryRow(
		t.Context(),
		`SELECT id FROM fry_read_pin WHERE receipt=$1 AND owner_member IS NULL`,
		receipt.ID.String(),
	).
		Scan(&parentID)
	requireReceiptNoError(t, err)
	memberTestReadRange(t, rpc, target, parentID)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin WHERE owner_member=$1`, f.members[0].id.String()) == 0,
		"member read not closed",
	)
}

func memberTestReadRange(t *testing.T, rpc memberconnect.MemberServiceClient, target *memberv1.Target, parent string) {
	t.Helper()
	stream, err := rpc.Read(
		t.Context(),
		&memberv1.ReadRequest{Target: target, Operation: newID(t).String(), ParentPin: parent, Offset: 2, Length: 7},
	)
	requireReceiptNoError(t, err)
	defer func() { requireReceiptNoError(t, stream.Close()) }()
	source := newMemberChunkReader(func() ([]byte, *memberv1.CompressedChunk, *memberv1.Completion, error) {
		frame, err := stream.Receive()
		return frame.GetData(), frame.GetCompressedData(), frame.GetCompletion(), memberReceiveError(err)
	}, 7, nil)
	data, err := io.ReadAll(source)
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, string(data) == "iginal ", "range differs", string(data))
}

func malformedMemberFrames(t *testing.T, kind string) []*memberv1.WriteRequest {
	t.Helper()
	if strings.HasPrefix(kind, "compressed_") {
		return malformedCompressedMemberFrames(t, kind)
	}

	return malformedRawMemberFrames(kind)
}

func malformedRawMemberFrames(kind string) []*memberv1.WriteRequest {
	data := &memberv1.WriteRequest{Frame: &memberv1.WriteRequest_Data{Data: []byte("broken")}}
	digest := sha256.Sum256([]byte("broken"))
	end := &memberv1.WriteRequest{
		Frame: &memberv1.WriteRequest_Completion{Completion: &memberv1.Completion{Size: 6, Sha256: digest[:]}},
	}
	switch kind {
	case "aggregate":
		frames := make([]*memberv1.WriteRequest, 0, 66)
		chunk := &memberv1.WriteRequest{Frame: &memberv1.WriteRequest_Data{Data: make([]byte, memberChunkSize)}}
		for range 65 {
			frames = append(frames, chunk)
		}
		return frames
	case "missing_completion":
		return []*memberv1.WriteRequest{data}
	case "digest":
		end.GetCompletion().Sha256[0] ^= 1
	case "size":
		end.GetCompletion().Size++
	case "after_completion":
		return []*memberv1.WriteRequest{data, end, data}
	case "duplicate_target":
		return []*memberv1.WriteRequest{{Frame: &memberv1.WriteRequest_Target{Target: &memberv1.Target{}}}}
	case "empty":
		return []*memberv1.WriteRequest{{Frame: &memberv1.WriteRequest_Data{Data: nil}}}
	case "oversized":
		return []*memberv1.WriteRequest{{Frame: &memberv1.WriteRequest_Data{Data: make([]byte, memberFrameLimit+1)}}}
	}
	return []*memberv1.WriteRequest{data, end}
}

func TestMemberGRPCMalformedWriteNeverPublishesCopy(t *testing.T) {
	f := setup(t)
	server, certs, url := memberHTTP(t, f)
	rpc := memberTestRPC(certs.allowed, url, memberRelease())
	for _, kind := range []string{
		"missing_completion",
		"digest",
		"size",
		"after_completion",
		"duplicate_target",
		"empty",
		"oversized",
		"aggregate",
		"compressed_size",
		"compressed_truncated",
		"compressed_trailing",
		"compressed_bomb",
		"compressed_empty",
		"compressed_missing_completion",
	} {
		t.Run(kind, func(t *testing.T) {
			c := memberTestClaim(t, f, server)
			stream, err := rpc.Write(t.Context())
			requireReceiptNoError(t, err)
			requireReceiptNoError(
				t,
				stream.Send(
					&memberv1.WriteRequest{
						Frame: &memberv1.WriteRequest_Target{Target: legacyTarget(replicaMetadata(c, server))},
					},
				),
			)
			for _, frame := range malformedMemberFrames(t, kind) {
				if err = stream.Send(frame); err != nil {
					break
				}
			}
			_, err = stream.CloseAndReceive()
			requireReceiptCondition(t, err != nil, "malformed stream acknowledged")
			requireReceiptCondition(
				t,
				scalar(
					t,
					f.pool,
					`SELECT count(*) FROM fry_target WHERE attempt=$1 AND locator IS NOT NULL`,
					c.Attempt.String(),
				) == 0,
				"malformed stream published",
			)
		})
	}
}

func waitMemberWrite(t *testing.T, m *LocalMember, attempt ID, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		stripe := m.stripe(attempt)
		stripe.Lock()
		active := stripe.active[attempt] != nil
		stripe.Unlock()
		if active == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("member write activity did not change", want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMemberGRPCCancelWaitsForActualWriteToStop(t *testing.T) {
	f := setup(t)
	server, certs, url := memberHTTP(t, f)
	rpc := memberTestRPC(certs.allowed, url, memberRelease())
	c := memberTestClaim(t, f, server)
	stream, err := rpc.Write(t.Context())
	requireReceiptNoError(t, err)
	target := legacyTarget(replicaMetadata(c, server))
	requireReceiptNoError(t, stream.Send(&memberv1.WriteRequest{Frame: &memberv1.WriteRequest_Target{Target: target}}))
	requireReceiptNoError(
		t,
		stream.Send(&memberv1.WriteRequest{Frame: &memberv1.WriteRequest_Data{Data: []byte("unfinished")}}),
	)
	waitMemberWrite(t, f.members[0], c.Attempt, true)
	requireReceiptNoError(t, f.s.db.abort(t.Context(), f.org, c.Attempt))
	_, err = rpc.Cancel(t.Context(), &memberv1.CancelRequest{Target: target})
	requireReceiptNoError(t, err)
	_, err = stream.CloseAndReceive()
	requireReceiptCondition(t, err != nil, "cancelled write acknowledged")
	waitMemberWrite(t, f.members[0], c.Attempt, false)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE attempt=$1 AND member=$2 AND cancel_confirmed`,
			c.Attempt.String(),
			f.members[0].id.String(),
		) == 1,
		"cancel acknowledgement preceded quiescence",
	)
}

func TestMemberGRPCDisconnectStopsIncompleteWrite(t *testing.T) {
	f := setup(t)
	server, certs, url := memberHTTP(t, f)
	rpc := memberTestRPC(certs.allowed, url, memberRelease())
	c := memberTestClaim(t, f, server)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := rpc.Write(ctx)
	requireReceiptNoError(t, err)
	requireReceiptNoError(
		t,
		stream.Send(
			&memberv1.WriteRequest{
				Frame: &memberv1.WriteRequest_Target{Target: legacyTarget(replicaMetadata(c, server))},
			},
		),
	)
	waitMemberWrite(t, f.members[0], c.Attempt, true)
	cancel()
	_, err = stream.CloseAndReceive()
	requireReceiptCondition(t, err != nil, "cancelled context acknowledged")
	waitMemberWrite(t, f.members[0], c.Attempt, false)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE attempt=$1 AND locator IS NOT NULL`,
			c.Attempt.String(),
		) == 0,
		"disconnect published copy",
	)
}

func TestMemberChunkReaderWithholdsFinalBytesOnInvalidCompletion(t *testing.T) {
	digest := sha256.Sum256([]byte("last"))
	frames := []*memberv1.ReadResponse{
		{Frame: &memberv1.ReadResponse_Data{Data: []byte("last")}},
		{Frame: &memberv1.ReadResponse_Completion{Completion: &memberv1.Completion{Size: 4, Sha256: digest[:]}}},
	}
	read := func() ([]byte, *memberv1.CompressedChunk, *memberv1.Completion, error) {
		if len(frames) == 0 {
			return nil, nil, nil, errors.New("lost grpc status")
		}
		next := frames[0]
		frames = frames[1:]
		return next.GetData(), next.GetCompressedData(), next.GetCompletion(), nil
	}
	source := newMemberChunkReader(read, 4, nil)
	var dst bytes.Buffer
	_, err := io.CopyN(&dst, source, 4)
	requireReceiptCondition(t, err != nil && dst.Len() == 0, "final bytes exposed without EOF")
}

func TestMemberGRPCStandardClientConnInvoke(t *testing.T) {
	f := setup(t)
	server, certs, url := memberHTTP(t, f)
	transport := receiptTestValue[*http.Transport](t, certs.allowed.Transport)
	tlsConfig := memberClientTLS(
		RemoteMemberOptions{
			TLS:       transport.TLSClientConfig,
			ServerKey: sha256.Sum256(certs.server.Certificates[0].Leaf.RawSubjectPublicKeyInfo),
		},
	)
	conn, err := grpc.NewClient(
		strings.TrimPrefix(url, "https://"),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(memberFrameLimit),
			grpc.MaxCallSendMsgSize(memberFrameLimit),
		),
	)
	requireReceiptNoError(t, err)
	defer func() { requireReceiptNoError(t, conn.Close()) }()
	ctx := metadata.AppendToOutgoingContext(t.Context(), "fry-release", memberRelease())
	wire := &memberv1.IdentityResponse{}
	requireReceiptNoError(
		t,
		conn.Invoke(ctx, memberconnect.MemberServiceIdentityProcedure, &memberv1.IdentityRequest{}, wire),
	)
	identity, err := identityFromProto(wire)
	requireReceiptNoError(t, err)
	requireReceiptCondition(
		t,
		identity.Member == f.members[0].id && identity.OwnerEpoch == server.owner.epoch,
		"standard gRPC identity differs",
	)
	wrong := metadata.AppendToOutgoingContext(t.Context(), "fry-release", memberRelease()+"-other")
	err = conn.Invoke(
		wrong,
		memberconnect.MemberServiceIdentityProcedure,
		&memberv1.IdentityRequest{},
		&memberv1.IdentityResponse{},
	)
	requireReceiptCondition(
		t,
		grpcstatus.Code(err) == codes.FailedPrecondition,
		"standard gRPC release mismatch admitted",
		err,
	)
}

func TestMemberGRPCRejectsOtherProtocolsAndCodecs(t *testing.T) {
	f := setup(t)
	_, certs, url := memberHTTP(t, f)
	for _, options := range [][]connecthttp.Option{
		{},
		{connecthttp.WithGRPCWeb()},
		{connecthttp.WithGRPC(), connecthttp.WithProtoJSON()},
	} {
		client := connect.NewClient(
			connecthttp.NewTransport(memberTestHTTPClient{certs.allowed, memberRelease()}, url, options...),
		)
		rpc := memberconnect.NewMemberServiceClient(client)
		_, err := rpc.Identity(t.Context(), &memberv1.IdentityRequest{})
		requireReceiptCondition(
			t,
			connect.CodeOf(err) == connect.CodeFailedPrecondition,
			"non-protobuf-gRPC request admitted",
			err,
		)
	}
}

func memberTestBlockedOriginal(t *testing.T, member *RemoteMember, organization ID) {
	t.Helper()
	reader, writer := io.Pipe()
	defer func() { requireReceiptNoError(t, writer.Close()) }()
	done := make(chan error, 1)
	c := claim{Attempt: newID(t), Receipt: newID(t), Organization: organization, Epoch: 1, Profile: 1}
	go func() { done <- member.write(t.Context(), nil, c, reader) }()
	select {
	case err := <-done:
		requireReceiptCondition(t, err != nil, "incomplete original acknowledged")
	case <-time.After(5 * time.Second):
		t.Fatal("terminal response left original producer blocked")
	}
}

func TestMemberGRPCRemoteRejectionClosesBlockedOriginal(t *testing.T) {
	f := setup(t)
	members, _ := remoteFixture(t, f)
	memberTestBlockedOriginal(t, members[0], f.org)
}

func TestMemberGRPCPrematureSuccessClosesBlockedOriginal(t *testing.T) {
	f := setup(t)
	wrapper := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/write" {
				next.ServeHTTP(w, r)
				return
			}
			// Named faulty-peer case: a syntactically valid empty protobuf success
			// response without consuming the original cannot count as write success.
			w.Header().Set("Content-Type", "application/grpc")
			w.Header().Set("Trailer", "Grpc-Status")
			if _, err := w.Write([]byte{0, 0, 0, 0, 0}); err != nil {
				t.Error(err)
			}
			w.Header().Set("Grpc-Status", "0")
		})
	}
	members, _ := remoteFixture(t, f, wrapper)
	memberTestBlockedOriginal(t, members[0], f.org)
}

func TestMemberGRPCOwnerCancellationInterruptsFlowControlledRead(t *testing.T) {
	f := setup(t)
	members, servers := remoteFixture(t, f)
	store, err := NewWithMembers(t.Context(), f.pool, [2]Member{members[0], members[1]}, f.s.db.opts)
	requireReceiptNoError(t, err)
	receipt, err := store.Accept(
		t.Context(),
		newID(t),
		f.envelope(t),
		io.NopCloser(&replicaPattern{remaining: 1 << 20}),
	)
	requireReceiptNoError(t, err)
	parent, err := store.Open(t.Context(), f.org, receipt.ID, 0, receipt.Size)
	requireReceiptNoError(t, err)
	defer func() { requireReceiptNoError(t, parent.Close()) }()
	var parentID string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(
			t.Context(),
			`SELECT id FROM fry_read_pin WHERE receipt=$1 AND owner_member IS NULL`,
			receipt.ID.String(),
		).
			Scan(&parentID),
	)
	conn, err := grpc.NewClient(
		strings.TrimPrefix(members[0].endpoint, "https://"),
		grpc.WithTransportCredentials(credentials.NewTLS(members[0].transport.TLSClientConfig.Clone())),
		grpc.WithInitialWindowSize(65535),
		grpc.WithInitialConnWindowSize(65535),
	)
	requireReceiptNoError(t, err)
	defer func() { requireReceiptNoError(t, conn.Close()) }()
	ctx, cancel := context.WithCancel(metadata.AppendToOutgoingContext(t.Context(), "fry-release", memberRelease()))
	defer cancel()
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, memberconnect.MemberServiceReadProcedure)
	requireReceiptNoError(t, err)
	operation := newID(t)
	req := &memberv1.ReadRequest{
		Target:    members[0].target(claim{Organization: f.org, Receipt: receipt.ID}),
		Operation: operation.String(),
		ParentPin: parentID,
		Length:    receipt.Size,
	}
	requireReceiptNoError(t, stream.SendMsg(req))
	requireReceiptNoError(t, stream.CloseSend())
	// Do not receive. The payload exceeds the HTTP/2 receive window, leaving an
	// actual storage reader blocked on network flow control until ownership ends.
	waitMemberReadState(t, f, operation, false)
	servers[0].owner.cancel()
	waitMemberReadState(t, f, operation, true)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin WHERE id=$1`, operation.String()) == 0,
		"cancelled member read retained a pin",
	)
}

func waitMemberReadState(t *testing.T, f *fixture, operation ID, closed bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_read WHERE id=$1 AND closed=$2`,
			operation.String(),
			closed,
		) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("member read closure state did not change", closed)
		}
		time.Sleep(time.Millisecond)
	}
}

func malformedCompressedMemberFrames(t *testing.T, kind string) []*memberv1.WriteRequest {
	t.Helper()
	encoder, err := newMemberChunkEncoder()
	requireReceiptNoError(t, err)
	encoded, err := encoder.encode(bytes.Repeat([]byte("A"), 16<<10))
	requireReceiptNoError(t, err)
	compressed := &memberv1.CompressedChunk{Size: encoded.size, Data: encoded.data}
	switch kind {
	case "compressed_size":
		compressed.Size = memberChunkSize + 1
	case "compressed_truncated":
		compressed.Data = compressed.Data[:len(compressed.Data)-2]
	case "compressed_trailing":
		compressed.Data = append(compressed.Data, 1, 2, 3)
	case "compressed_bomb":
		compressed.Size = 1
	case "compressed_empty":
		compressed.Data = nil
	}
	frame := memberWriteFrame(nil, compressed, nil)
	if kind == "compressed_missing_completion" {
		return []*memberv1.WriteRequest{frame}
	}
	digest := sha256.Sum256(bytes.Repeat([]byte("A"), 16<<10))
	return []*memberv1.WriteRequest{
		frame,
		memberWriteFrame(nil, nil, &memberv1.Completion{Size: 16 << 10, Sha256: digest[:]}),
	}
}
