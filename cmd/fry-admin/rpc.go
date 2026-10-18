package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/cotocisternas/fry/gen/admin/go/fry/admin/v1/adminv1connect"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type rpcCommand func(context.Context, []byte) (proto.Message, error)

func commandRPC[Req any, Res proto.Message, P interface {
	*Req
	proto.Message
}](call func(context.Context, P) (Res, error)) rpcCommand {
	return func(ctx context.Context, input []byte) (proto.Message, error) {
		request := P(new(Req))
		if err := protojson.Unmarshal(input, request); err != nil {
			return nil, fmt.Errorf("invalid command fields: %w", err)
		}
		return call(ctx, request)
	}
}

func (c *client) rpcCommands() map[string]rpcCommand {
	codec := connectproto.NewJSONCodec()
	codec.MarshalOptions.UseProtoNames = true
	codec.MarshalOptions.EmitDefaultValues = true
	rpc := adminv1connect.NewAdminServiceClient(
		connect.NewClient(connecthttp.NewTransport(rpcHTTPClient{c.http}, c.origin,
			connecthttp.WithSendCodec(connect.CodecNameJSON), connecthttp.WithCodecs(codec),
			connecthttp.WithReadMaxBytes(1<<20), connecthttp.WithSendMaxBytes(1<<20))),
	)
	return map[string]rpcCommand{
		"GET config":               commandRPC(rpc.GetConfig),
		"POST login":               commandRPC(rpc.Login),
		"GET session":              commandRPC(rpc.GetSession),
		"POST logout":              commandRPC(rpc.Logout),
		"GET organizations":        commandRPC(rpc.ListOrganizations),
		"GET inboxes":              commandRPC(rpc.ListInboxes),
		"GET sender-grants":        commandRPC(rpc.ListSenderGrants),
		"GET domains":              commandRPC(rpc.ListDomains),
		"GET aliases":              commandRPC(rpc.ListAliases),
		"GET mail-accounts":        commandRPC(rpc.ListMailAccounts),
		"GET submission-accounts":  commandRPC(rpc.ListSubmissionAccounts),
		"GET messages":             commandRPC(rpc.ListMessages),
		"GET audit":                commandRPC(rpc.ListAudits),
		"GET users":                commandRPC(rpc.ListUsers),
		"POST organizations":       commandRPC(rpc.PutOrganization),
		"POST inboxes":             commandRPC(rpc.CreateInbox),
		"POST domains":             commandRPC(rpc.PutDomain),
		"POST aliases":             commandRPC(rpc.PutAlias),
		"POST mail-accounts":       commandRPC(rpc.PutMailAccount),
		"POST sender-challenge":    commandRPC(rpc.CreateSenderChallenge),
		"POST sender-verify":       commandRPC(rpc.VerifySender),
		"POST sender-revoke":       commandRPC(rpc.RevokeSender),
		"POST sender-override":     commandRPC(rpc.OverrideSender),
		"POST mail-access":         commandRPC(rpc.SetMailAccess),
		"GET member-storage":       commandRPC(rpc.GetMemberStorage),
		"GET storage-usage":        commandRPC(rpc.GetStorageUsage),
		"POST storage-quota":       commandRPC(rpc.SetStorageQuota),
		"POST submission-accounts": commandRPC(rpc.CreateSubmissionAccount),
		"POST submission-policy":   commandRPC(rpc.SetSubmissionPolicy),
		"POST submission-password": commandRPC(rpc.RotateSubmissionPassword),
		"POST submission-access":   commandRPC(rpc.SetSubmissionAccess),
		"POST users":               commandRPC(rpc.PutUser),
		"POST grants":              commandRPC(rpc.ReplaceGrants),
		"POST access":              commandRPC(rpc.SetEnabled),
	}
}

func commandInput(query url.Values, data any) ([]byte, error) {
	if data != nil {
		return json.Marshal(data)
	}
	fields := map[string]any{}
	for key, values := range query {
		value := values[0]
		if key == "limit" {
			number, err := strconv.ParseInt(value, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid page limit")
			}
			fields[key] = number
		} else {
			fields[key] = value
		}
	}
	return json.Marshal(fields)
}

func (c *client) call(ctx context.Context, method, path string, data any) (json.RawMessage, error) {
	target, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	command, ok := c.rpcCommands()[method+" "+target.Path]
	if !ok {
		return nil, fmt.Errorf("unknown administration operation")
	}
	input, err := commandInput(target.Query(), data)
	if err != nil {
		return nil, err
	}
	ctx, info := connect.NewClientContext(ctx)
	info.RequestHeader().Set("Origin", c.origin)
	info.RequestHeader().Set("X-FRY-CSRF", c.csrf)
	result, err := command(ctx, input)
	if err != nil {
		return nil, err
	}
	raw, err := (protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}).Marshal(result)
	if err != nil {
		return nil, err
	}
	return commandOutput(raw)
}

func commandOutput(raw []byte) (json.RawMessage, error) {
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, err
	}
	for _, field := range []string{"result", "items"} {
		if result, ok := wrapper[field]; ok {
			return result, nil
		}
	}
	return raw, nil
}

// A response cleanup error cannot undo a server-acknowledged mutation. Read and
// framing errors still propagate through the generated client's decoder.
type (
	rpcHTTPClient struct{ client *http.Client }
	rpcBody       struct{ io.ReadCloser }
)

func (body rpcBody) Close() error { warnClose(body.ReadCloser, "administration response"); return nil }

func (c rpcHTTPClient) Do(request *http.Request) (*http.Response, error) {
	// #nosec G704 -- The generated client constructs this request using signIn's validated HTTPS or numeric-loopback
	// origin; redirects are refused. Owner: admin maintainers; review by 2027-01-18.
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	response.Body = rpcBody{response.Body}
	return response, nil
}
