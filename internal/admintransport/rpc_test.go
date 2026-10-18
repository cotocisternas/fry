//go:build linux

package admintransport

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cotocisternas/fry/gen/admin/go/fry/admin/v1/adminv1connect"
	"github.com/cotocisternas/fry/internal/admin"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	adminv1 "github.com/cotocisternas/fry/gen/admin/go/fry/admin/v1"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
)

type rpcFixture struct {
	f      *fixture
	server *httptest.Server
	client *http.Client
	rpc    adminv1connect.AdminServiceClient
	csrf   string
}

func newRPCFixture(t *testing.T, f *fixture, grpc bool) *rpcFixture {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	h, err := NewHTTP(f.service, HTTPOptions{Origin: "https://" + server.Listener.Addr().String()})
	check(t, err)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if grpc && strings.HasPrefix(r.URL.Path, "/fry.admin.") && r.ProtoMajor != 2 {
			t.Error("gRPC did not use HTTP/2")
		}
		h.ServeHTTP(w, r)
	})
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	client := server.Client()
	client.Jar, err = cookiejar.New(nil)
	check(t, err)
	opts := []connecthttp.Option{connecthttp.WithReadMaxBytes(1 << 20)}
	if grpc {
		opts = append(opts, connecthttp.WithGRPC())
	}
	rpc := adminv1connect.NewAdminServiceClient(
		connect.NewClient(connecthttp.NewTransport(client, server.URL, opts...)),
	)
	result := &rpcFixture{f: f, server: server, client: client, rpc: rpc}
	login, err := rpc.Login(
		result.context(t),
		&adminv1.LoginRequest{Username: "operator", Password: "test-operator-secret"},
	)
	check(t, err)
	result.csrf = login.Result.Csrf
	return result
}

func (r *rpcFixture) context(t *testing.T) context.Context {
	t.Helper()
	ctx, info := connect.NewClientContext(t.Context())
	info.RequestHeader().Set("Origin", r.server.URL)
	info.RequestHeader().Set("X-FRY-CSRF", r.csrf)
	return ctx
}

func requireRPCCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if got := connect.CodeOf(err); got != want {
		t.Fatalf("RPC code=%s, want %s; err=%v", got, want, err)
	}
}

func TestTypedAdminRPCDirectoryOverGRPCHTTP2(t *testing.T) {
	r := newRPCFixture(t, setup(t), true)
	ctx := r.context(t)
	config, err := r.rpc.GetConfig(ctx, &adminv1.GetConfigRequest{})
	check(t, err)
	if config.Oidc {
		t.Fatal("unexpected OIDC")
	}
	session, err := r.rpc.GetSession(r.context(t), &adminv1.GetSessionRequest{})
	check(t, err)
	if session.Result.Csrf != r.csrf {
		t.Fatal("session CSRF changed")
	}
	org, err := r.rpc.PutOrganization(r.context(t), &adminv1.PutOrganizationRequest{Name: "RPC organization"})
	check(t, err)
	orgs, err := r.rpc.ListOrganizations(r.context(t), &adminv1.ListOrganizationsRequest{})
	check(t, err)
	if len(orgs.Items) != 3 {
		t.Fatal("organization missing")
	}
	inbox, err := r.rpc.CreateInbox(r.context(t), &adminv1.CreateInboxRequest{Organization: org.Result.Id})
	check(t, err)
	inboxes, err := r.rpc.ListInboxes(r.context(t), &adminv1.ListInboxesRequest{Organization: org.Result.Id})
	check(t, err)
	if len(inboxes.Items) != 1 || inboxes.Items[0].Id != inbox.Result.Id {
		t.Fatal("inbox mapping lost")
	}
	r.checkDirectory(t, org.Result.Id, inbox.Result.Id)
}

func (r *rpcFixture) checkDirectory(t *testing.T, org, inbox string) {
	t.Helper()
	_, err := r.rpc.PutDomain(
		r.context(t),
		&adminv1.PutDomainRequest{Name: "rpc.example.test", Organization: org, CatchAll: inbox, Enabled: true},
	)
	check(t, err)
	domains, err := r.rpc.ListDomains(r.context(t), &adminv1.ListDomainsRequest{Organization: org})
	check(t, err)
	if len(domains.Items) != 1 || domains.Items[0].CatchAll != inbox {
		t.Fatal("domain mapping")
	}
	_, err = r.rpc.PutAlias(
		r.context(t),
		&adminv1.PutAliasRequest{
			Address:      "alias@rpc.example.test",
			Organization: org,
			Inboxes:      []string{inbox},
			Enabled:      true,
		},
	)
	check(t, err)
	aliases, err := r.rpc.ListAliases(r.context(t), &adminv1.ListAliasesRequest{Organization: org})
	check(t, err)
	if len(aliases.Items) != 1 || len(aliases.Items[0].Inboxes) != 1 {
		t.Fatal("alias mapping")
	}
	// Owner: admin maintainers; review by 2027-01-18.
	// #nosec G101 -- Disposable synthetic credentials used only in the real-PostgreSQL test fixture.
	_, err = r.rpc.PutMailAccount(
		r.context(t),
		&adminv1.PutMailAccountRequest{
			Organization: org,
			Username:     "rpc-imap",
			Password:     "rpc-mail-secret",
			Inbox:        inbox,
			Enabled:      true,
		},
	)
	check(t, err)
	accounts, err := r.rpc.ListMailAccounts(r.context(t), &adminv1.ListMailAccountsRequest{Organization: org})
	check(t, err)
	if len(accounts.Items) != 1 || !accounts.Items[0].Enabled {
		t.Fatal("mail credential mapping")
	}
	_, err = r.rpc.SetMailAccess(
		r.context(t),
		&adminv1.SetMailAccessRequest{Organization: org, Username: "rpc-imap", Enabled: false},
	)
	check(t, err)
}

func TestTypedAdminRPCSubmissionAndStorage(t *testing.T) {
	r := newRPCFixture(t, setup(t), false)
	org := r.f.org
	// Owner: admin maintainers; review by 2027-01-18.
	// #nosec G101 -- Disposable synthetic credentials used only in the real-PostgreSQL test fixture.
	_, err := r.rpc.CreateSubmissionAccount(
		r.context(t),
		&adminv1.CreateSubmissionAccountRequest{
			Organization:      org,
			Username:          "rpc-smtp",
			Password:          "rpc-smtp-secret",
			AuthorizedDomains: []string{"example.test"},
			Enabled:           true,
		},
	)
	check(t, err)
	_, err = r.rpc.SetSubmissionPolicy(
		r.context(t),
		&adminv1.SetSubmissionPolicyRequest{
			Organization:      org,
			Username:          "rpc-smtp",
			AuthorizedSenders: []string{"sender@example.test"},
		},
	)
	check(t, err)
	// Owner: admin maintainers; review by 2027-01-18.
	// #nosec G101 -- Disposable synthetic credentials used only in the real-PostgreSQL test fixture.
	_, err = r.rpc.RotateSubmissionPassword(
		r.context(t),
		&adminv1.RotateSubmissionPasswordRequest{
			Organization: org,
			Username:     "rpc-smtp",
			Password:     "rpc-rotated-secret",
		},
	)
	check(t, err)
	_, err = r.rpc.SetSubmissionAccess(
		r.context(t),
		&adminv1.SetSubmissionAccessRequest{Organization: org, Username: "rpc-smtp", Enabled: false},
	)
	check(t, err)
	accounts, err := r.rpc.ListSubmissionAccounts(
		r.context(t),
		&adminv1.ListSubmissionAccountsRequest{Organization: org},
	)
	check(t, err)
	if len(accounts.Items) != 1 || accounts.Items[0].Enabled ||
		accounts.Items[0].AuthorizedSenders[0] != "sender@example.test" {
		t.Fatal("submission changes lost")
	}
	limit := "9223372036854775807"
	_, err = r.rpc.SetStorageQuota(r.context(t), &adminv1.SetStorageQuotaRequest{Organization: org, LimitBytes: &limit})
	check(t, err)
	usage, err := r.rpc.GetStorageUsage(r.context(t), &adminv1.GetStorageUsageRequest{Organization: org})
	check(t, err)
	if usage.Result.GetLimitBytes() != limit {
		t.Fatal("quota precision lost")
	}
	_, err = r.rpc.SetStorageQuota(r.context(t), &adminv1.SetStorageQuotaRequest{Organization: org})
	check(t, err)
	snapshot, err := r.rpc.GetMemberStorage(r.context(t), &adminv1.GetMemberStorageRequest{Member: r.f.member})
	check(t, err)
	if snapshot.Result.Member != r.f.member || snapshot.Result.Profile == nil {
		t.Fatal("member snapshot missing")
	}
}

func TestTypedAdminRPCSenderAndAdministration(t *testing.T) {
	var txt string
	f := setupWithDNS(t, senderDNSFunc(func(context.Context, string) ([]string, error) { return []string{txt}, nil }))
	r := newRPCFixture(t, f, false)
	challenge, err := r.rpc.CreateSenderChallenge(
		r.context(t),
		&adminv1.CreateSenderChallengeRequest{Organization: f.org, Scope: "example.test"},
	)
	check(t, err)
	txt = challenge.Result.TxtValue
	verified, err := r.rpc.VerifySender(
		r.context(t),
		&adminv1.VerifySenderRequest{Organization: f.org, Challenge: challenge.Result.Id},
	)
	check(t, err)
	if !verified.Result.Active {
		t.Fatal("verification not active")
	}
	_, err = r.rpc.OverrideSender(
		r.context(t),
		&adminv1.OverrideSenderRequest{Organization: f.org, Scope: "override.test", Reason: "RPC regression"},
	)
	check(t, err)
	_, err = r.rpc.RevokeSender(
		r.context(t),
		&adminv1.RevokeSenderRequest{Organization: f.org, Scope: "override.test", Reason: "RPC revoke"},
	)
	check(t, err)
	grants, err := r.rpc.ListSenderGrants(r.context(t), &adminv1.ListSenderGrantsRequest{Organization: f.org})
	check(t, err)
	if len(grants.Items) != 2 {
		t.Fatal("sender grants missing")
	}
	r.checkAdministrator(t)
}

func (r *rpcFixture) checkAdministrator(t *testing.T) {
	t.Helper()
	// Owner: admin maintainers; review by 2027-01-18.
	// #nosec G101 -- Disposable synthetic credentials used only in the real-PostgreSQL test fixture.
	user, err := r.rpc.PutUser(
		r.context(t),
		&adminv1.PutUserRequest{Username: "rpc-manager", Password: "rpc-manager-secret", Enabled: true},
	)
	check(t, err)
	_, err = r.rpc.ReplaceGrants(
		r.context(t),
		&adminv1.ReplaceGrantsRequest{User: user.Id, Grants: []*adminv1.Grant{{Scope: r.f.org}}},
	)
	check(t, err)
	_, err = r.rpc.SetEnabled(r.context(t), &adminv1.SetEnabledRequest{User: user.Id, Enabled: false})
	check(t, err)
	users, err := r.rpc.ListUsers(r.context(t), &adminv1.ListUsersRequest{})
	check(t, err)
	if len(users.Items) != 2 {
		t.Fatal("administrator missing")
	}
	audits, err := r.rpc.ListAudits(r.context(t), &adminv1.ListAuditsRequest{})
	check(t, err)
	if len(audits.Items) == 0 {
		t.Fatal("mutation audit missing")
	}
}

func TestTypedAdminRPCContentSecurityAndRevocation(t *testing.T) {
	r := newRPCFixture(t, setup(t), true)
	receipt, original := r.f.receive(t)
	messages, err := r.rpc.ListMessages(r.context(t), &adminv1.ListMessagesRequest{Organization: r.f.org})
	check(t, err)
	if len(messages.Items) != 1 || messages.Items[0].Id != receipt {
		t.Fatal("message metadata missing")
	}
	response, raw := request(
		t,
		r.client,
		r.server.URL,
		"GET",
		"/api/content?organization="+r.f.org+"&receipt="+receipt,
		"",
		nil,
	)
	if response.StatusCode != 200 || !bytes.Equal(raw, original) {
		t.Fatal("original bytes changed")
	}
	csrf := r.csrf
	r.csrf = ""
	_, err = r.rpc.PutOrganization(r.context(t), &adminv1.PutOrganizationRequest{Name: "denied"})
	requireRPCCode(t, err, connect.CodePermissionDenied)
	r.csrf = csrf
	_, err = r.rpc.Logout(r.context(t), &adminv1.LogoutRequest{})
	check(t, err)
	_, err = r.rpc.ListMessages(r.context(t), &adminv1.ListMessagesRequest{Organization: r.f.org})
	requireRPCCode(t, err, connect.CodeUnauthenticated)
	response, _ = request(
		t,
		r.client,
		r.server.URL,
		"GET",
		"/api/content?organization="+r.f.org+"&receipt="+receipt,
		"",
		nil,
	)
	requireHTTPStatus(t, response, 401)
}

func TestTypedAdminRPCRejectsMalformedAndOversizeRequests(t *testing.T) {
	r := newRPCFixture(t, setup(t), false)
	for _, body := range []string{
		`{"name":"unknown","unknown":true}`,
		`{"name":"` + strings.Repeat("a", 1<<20) + `"}`,
	} {
		req, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			r.server.URL+adminv1connect.AdminServicePutOrganizationProcedure,
			strings.NewReader(body),
		)
		check(t, err)
		req.Header.Set("Origin", r.server.URL)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-FRY-CSRF", r.csrf)
		response, err := r.client.Do(req)
		check(t, err)
		check(t, response.Body.Close())
		if response.StatusCode < 400 {
			t.Fatal("malformed mutation accepted")
		}
	}
	orgs, err := r.f.service.Organizations(t.Context(), r.f.root.Token)
	check(t, err)
	if len(orgs) != 2 {
		t.Fatal("rejected request changed durable state")
	}
}

var (
	_ io.Reader
	_ admin.Identity
)

func TestTypedAdminRPCRejectsUnknownBinaryMutation(t *testing.T) {
	r := newRPCFixture(t, setup(t), true)
	input := &adminv1.PutOrganizationRequest{Name: "unknown binary field"}
	input.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	_, err := r.rpc.PutOrganization(r.context(t), input)
	requireRPCCode(t, err, connect.CodeInvalidArgument)
}

func TestTypedAdminRPCStandardGRPCClient(t *testing.T) {
	r := newRPCFixture(t, setup(t), true)
	transport, ok := r.server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("test TLS transport missing")
	}
	tlsConfig := transport.TLSClientConfig.Clone()
	connection, err := grpc.NewClient(
		strings.TrimPrefix(r.server.URL, "https://"),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
	)
	check(t, err)
	t.Cleanup(func() { check(t, connection.Close()) })
	origin, err := url.Parse(r.server.URL)
	check(t, err)
	cookies := r.client.Jar.Cookies(origin)
	if len(cookies) != 1 {
		t.Fatal("missing authenticated session cookie")
	}
	ctx := metadata.NewOutgoingContext(
		t.Context(),
		metadata.Pairs("origin", r.server.URL, "cookie", cookies[0].String(), "x-fry-csrf", r.csrf),
	)
	var created adminv1.PutOrganizationResponse
	check(
		t,
		connection.Invoke(
			ctx,
			adminv1connect.AdminServicePutOrganizationProcedure,
			&adminv1.PutOrganizationRequest{Name: "standard grpc-go"},
			&created,
		),
	)
	if !validResponseID(created.GetResult().GetId()) {
		t.Fatal("standard gRPC mutation did not return typed identity")
	}
	var organizations adminv1.ListOrganizationsResponse
	check(
		t,
		connection.Invoke(
			ctx,
			adminv1connect.AdminServiceListOrganizationsProcedure,
			&adminv1.ListOrganizationsRequest{},
			&organizations,
		),
	)
	if len(organizations.Items) != 3 {
		t.Fatal("standard gRPC mutation missing from durable state")
	}
}

func TestTypedAdminRPCRechecksScopeAndRevokedAccess(t *testing.T) {
	r := newRPCFixture(t, setup(t), true)
	manager := r.f.manager(t, false)
	origin, err := url.Parse(r.server.URL)
	check(t, err)
	r.client.Jar.SetCookies(
		origin,
		[]*http.Cookie{
			{
				Name:     "__Host-fry-admin",
				Value:    manager.Token,
				Path:     "/",
				Secure:   true,
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			},
		},
	)
	r.csrf = manager.Identity.CSRF
	_, err = r.rpc.ListInboxes(r.context(t), &adminv1.ListInboxesRequest{Organization: r.f.org})
	check(t, err)
	_, err = r.rpc.ListInboxes(r.context(t), &adminv1.ListInboxesRequest{Organization: r.f.other})
	requireRPCCode(t, err, connect.CodePermissionDenied)
	_, err = r.rpc.ListUsers(r.context(t), &adminv1.ListUsersRequest{})
	requireRPCCode(t, err, connect.CodePermissionDenied)
	_, err = r.rpc.GetMemberStorage(r.context(t), &adminv1.GetMemberStorageRequest{Member: r.f.member})
	requireRPCCode(t, err, connect.CodePermissionDenied)
	_, err = r.rpc.PutMailAccount(
		r.context(t),
		&adminv1.PutMailAccountRequest{Organization: r.f.org, Username: "denied"},
	)
	requireRPCCode(t, err, connect.CodePermissionDenied)
	check(t, r.f.service.SetEnabled(t.Context(), r.f.root.Token, manager.Identity.ID, false))
	_, err = r.rpc.ListInboxes(r.context(t), &adminv1.ListInboxesRequest{Organization: r.f.org})
	requireRPCCode(t, err, connect.CodeUnauthenticated)
}
