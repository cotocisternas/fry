//go:build linux

package admintransport

import (
	"context"

	adminv1 "github.com/cotocisternas/fry/gen/admin/go/fry/admin/v1"
	"github.com/cotocisternas/fry/internal/admin"
	"github.com/cotocisternas/fry/internal/maildirectory"

	"connectrpc.com/connect/v2"
)

type rpcService struct{ h *HTTP }

func rpcResult[T any](result *T, err error) (*T, error) {
	if err != nil {
		return nil, rpcError(err)
	}
	return result, nil
}

func (s *rpcService) GetConfig(ctx context.Context, r *adminv1.GetConfigRequest) (*adminv1.GetConfigResponse, error) {
	h := s.h
	return &adminv1.GetConfigResponse{Oidc: h.oidc != nil}, nil
}

func (s *rpcService) Login(ctx context.Context, r *adminv1.LoginRequest) (*adminv1.LoginResponse, error) {
	h := s.h
	session, err := h.service.Login(ctx, r.Username, r.Password)
	if err != nil {
		return nil, rpcError(err)
	}
	info, _ := connect.CallInfoForServerContext(ctx)
	info.ResponseHeader().Add("Set-Cookie", h.sessionCookie(session).String())
	return &adminv1.LoginResponse{Result: toIdentity(session.Identity)}, nil
}

func (s *rpcService) GetSession(
	ctx context.Context,
	r *adminv1.GetSessionRequest,
) (*adminv1.GetSessionResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	value, err := h.service.Identity(ctx, token)
	return rpcResult(&adminv1.GetSessionResponse{Result: toIdentity(value)}, err)
}

func (s *rpcService) Logout(ctx context.Context, r *adminv1.LogoutRequest) (*adminv1.LogoutResponse, error) {
	h := s.h
	err := h.service.Logout(ctx, h.rpcToken(ctx))
	if err != nil {
		return nil, rpcError(err)
	}
	info, _ := connect.CallInfoForServerContext(ctx)
	// Owner: admin maintainers; review by 2027-01-18.
	// #nosec G124 -- sessionCookie sets Secure from the validated origin and always sets HttpOnly and SameSite.
	cookie := h.sessionCookie(admin.Session{})
	cookie.MaxAge = -1
	info.ResponseHeader().Add("Set-Cookie", cookie.String())
	return &adminv1.LogoutResponse{Ok: true}, nil
}

func (s *rpcService) ListOrganizations(
	ctx context.Context,
	r *adminv1.ListOrganizationsRequest,
) (*adminv1.ListOrganizationsResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	value, err := h.service.Organizations(ctx, token)
	return rpcResult(&adminv1.ListOrganizationsResponse{Items: mapValues(value, toOrganization)}, err)
}

func (s *rpcService) ListInboxes(
	ctx context.Context,
	r *adminv1.ListInboxesRequest,
) (*adminv1.ListInboxesResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	value, err := h.service.Inboxes(ctx, token, r.Organization)
	return rpcResult(&adminv1.ListInboxesResponse{Items: mapValues(value, toInbox)}, err)
}

func (s *rpcService) ListSenderGrants(
	ctx context.Context,
	r *adminv1.ListSenderGrantsRequest,
) (*adminv1.ListSenderGrantsResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	value, err := h.service.SenderGrants(ctx, token, r.Organization)
	return rpcResult(&adminv1.ListSenderGrantsResponse{Items: mapValues(value, toSenderGrant)}, err)
}

func (s *rpcService) ListDomains(
	ctx context.Context,
	r *adminv1.ListDomainsRequest,
) (*adminv1.ListDomainsResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	value, err := h.service.Domains(ctx, token, r.Organization)
	return rpcResult(&adminv1.ListDomainsResponse{Items: mapValues(value, toDomain)}, err)
}

func (s *rpcService) ListAliases(
	ctx context.Context,
	r *adminv1.ListAliasesRequest,
) (*adminv1.ListAliasesResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	value, err := h.service.Aliases(ctx, token, r.Organization)
	return rpcResult(&adminv1.ListAliasesResponse{Items: mapValues(value, toAlias)}, err)
}

func (s *rpcService) ListMailAccounts(
	ctx context.Context,
	r *adminv1.ListMailAccountsRequest,
) (*adminv1.ListMailAccountsResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	value, err := h.service.MailAccounts(ctx, token, r.Organization)
	return rpcResult(&adminv1.ListMailAccountsResponse{Items: mapValues(value, toAccount)}, err)
}

func (s *rpcService) ListSubmissionAccounts(
	ctx context.Context,
	r *adminv1.ListSubmissionAccountsRequest,
) (*adminv1.ListSubmissionAccountsResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	value, err := h.service.SubmissionAccounts(ctx, token, r.Organization)
	return rpcResult(&adminv1.ListSubmissionAccountsResponse{Items: mapValues(value, toSubmissionAccount)}, err)
}

func (s *rpcService) ListMessages(
	ctx context.Context,
	r *adminv1.ListMessagesRequest,
) (*adminv1.ListMessagesResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	value, err := h.service.Messages(ctx, token, r.Organization, r.After)
	return rpcResult(&adminv1.ListMessagesResponse{Items: mapValues(value, toMessage)}, err)
}

func (s *rpcService) ListAudits(
	ctx context.Context,
	r *adminv1.ListAuditsRequest,
) (*adminv1.ListAuditsResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	value, err := h.service.Audits(ctx, token, r.Before)
	return rpcResult(&adminv1.ListAuditsResponse{Items: mapValues(value, toAudit)}, err)
}

func (s *rpcService) ListUsers(ctx context.Context, r *adminv1.ListUsersRequest) (*adminv1.ListUsersResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	value, err := h.service.Users(ctx, token)
	return rpcResult(&adminv1.ListUsersResponse{Items: mapValues(value, toUser)}, err)
}

func (s *rpcService) PutOrganization(
	ctx context.Context,
	r *adminv1.PutOrganizationRequest,
) (*adminv1.PutOrganizationResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	if r.Id == "" {
		id, err := identifier()
		if err != nil {
			return nil, rpcError(err)
		}
		r.Id = id
	}
	err := h.service.PutOrganization(ctx, token, fromOrganization(r))
	return rpcResult(&adminv1.PutOrganizationResponse{Result: toOrganization(fromOrganization(r))}, err)
}

func (s *rpcService) CreateInbox(
	ctx context.Context,
	r *adminv1.CreateInboxRequest,
) (*adminv1.CreateInboxResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	if r.Id == "" {
		id, err := identifier()
		if err != nil {
			return nil, rpcError(err)
		}
		r.Id = id
	}
	err := h.service.CreateInbox(ctx, token, fromInbox(r))
	return rpcResult(&adminv1.CreateInboxResponse{Result: toInbox(fromInbox(r))}, err)
}

func (s *rpcService) PutDomain(ctx context.Context, r *adminv1.PutDomainRequest) (*adminv1.PutDomainResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	err := h.service.PutDomain(ctx, token, fromDomain(r))
	return rpcResult(&adminv1.PutDomainResponse{Result: toDomain(fromDomain(r))}, err)
}

func (s *rpcService) PutAlias(ctx context.Context, r *adminv1.PutAliasRequest) (*adminv1.PutAliasResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	err := h.service.PutAlias(ctx, token, fromAlias(r))
	return rpcResult(&adminv1.PutAliasResponse{Result: toAlias(fromAlias(r))}, err)
}

func (s *rpcService) PutMailAccount(
	ctx context.Context,
	r *adminv1.PutMailAccountRequest,
) (*adminv1.PutMailAccountResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	err := h.service.PutMailAccount(ctx, token, fromAccount(r), r.Password)
	return rpcResult(&adminv1.PutMailAccountResponse{Result: toAccount(fromAccount(r))}, err)
}

func (s *rpcService) CreateSenderChallenge(
	ctx context.Context,
	r *adminv1.CreateSenderChallengeRequest,
) (*adminv1.CreateSenderChallengeResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	value, err := h.service.SenderChallenge(ctx, token, r.Organization, r.Scope)
	return rpcResult(&adminv1.CreateSenderChallengeResponse{Result: toSenderChallenge(value)}, err)
}

func (s *rpcService) VerifySender(
	ctx context.Context,
	r *adminv1.VerifySenderRequest,
) (*adminv1.VerifySenderResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	value, err := h.service.VerifySender(ctx, token, r.Organization, r.Challenge)
	return rpcResult(&adminv1.VerifySenderResponse{Result: toSenderGrant(value)}, err)
}

func (s *rpcService) RevokeSender(
	ctx context.Context,
	r *adminv1.RevokeSenderRequest,
) (*adminv1.RevokeSenderResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	err := h.service.RevokeSender(ctx, token, r.Organization, r.Scope, r.Reason)
	return rpcResult(&adminv1.RevokeSenderResponse{Ok: err == nil}, err)
}

func (s *rpcService) OverrideSender(
	ctx context.Context,
	r *adminv1.OverrideSenderRequest,
) (*adminv1.OverrideSenderResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	value, err := h.service.OverrideSender(ctx, token, r.Organization, r.Scope, r.Reason)
	return rpcResult(&adminv1.OverrideSenderResponse{Result: toSenderGrant(value)}, err)
}

func (s *rpcService) SetMailAccess(
	ctx context.Context,
	r *adminv1.SetMailAccessRequest,
) (*adminv1.SetMailAccessResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	err := h.service.SetMailAccess(ctx, token, r.Organization, r.Username, r.Enabled)
	return rpcResult(&adminv1.SetMailAccessResponse{Ok: err == nil}, err)
}

func (s *rpcService) GetMemberStorage(
	ctx context.Context,
	r *adminv1.GetMemberStorageRequest,
) (*adminv1.GetMemberStorageResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	if r.Limit == 0 {
		r.Limit = 64
	}
	value, err := h.service.MemberStorage(ctx, token, r.Member, r.After, int(r.Limit))
	return rpcResult(&adminv1.GetMemberStorageResponse{Result: toMemberStorageSnapshot(value)}, err)
}

func (s *rpcService) GetStorageUsage(
	ctx context.Context,
	r *adminv1.GetStorageUsageRequest,
) (*adminv1.GetStorageUsageResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	value, err := h.service.StorageUsage(ctx, token, r.Organization)
	return rpcResult(&adminv1.GetStorageUsageResponse{Result: toStorageUsage(value)}, err)
}

func (s *rpcService) SetStorageQuota(
	ctx context.Context,
	r *adminv1.SetStorageQuotaRequest,
) (*adminv1.SetStorageQuotaResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	err := h.service.SetStorageQuota(ctx, token, r.Organization, r.LimitBytes)
	return rpcResult(&adminv1.SetStorageQuotaResponse{Ok: err == nil}, err)
}

func (s *rpcService) CreateSubmissionAccount(
	ctx context.Context,
	r *adminv1.CreateSubmissionAccountRequest,
) (*adminv1.CreateSubmissionAccountResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	err := h.service.CreateSubmissionAccount(
		ctx,
		token,
		maildirectory.SubmissionAccount{
			Organization:      r.Organization,
			Username:          r.Username,
			Enabled:           r.Enabled,
			AuthorizedSenders: r.AuthorizedSenders,
			AuthorizedDomains: r.AuthorizedDomains,
		},
		r.Password,
	)
	return rpcResult(&adminv1.CreateSubmissionAccountResponse{Ok: err == nil}, err)
}

func (s *rpcService) SetSubmissionPolicy(
	ctx context.Context,
	r *adminv1.SetSubmissionPolicyRequest,
) (*adminv1.SetSubmissionPolicyResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	err := h.service.SetSubmissionPolicy(
		ctx,
		token,
		r.Organization,
		r.Username,
		r.AuthorizedSenders,
		r.AuthorizedDomains,
	)
	return rpcResult(&adminv1.SetSubmissionPolicyResponse{Ok: err == nil}, err)
}

func (s *rpcService) RotateSubmissionPassword(
	ctx context.Context,
	r *adminv1.RotateSubmissionPasswordRequest,
) (*adminv1.RotateSubmissionPasswordResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	err := h.service.RotateSubmissionPassword(ctx, token, r.Organization, r.Username, r.Password)
	return rpcResult(&adminv1.RotateSubmissionPasswordResponse{Ok: err == nil}, err)
}

func (s *rpcService) SetSubmissionAccess(
	ctx context.Context,
	r *adminv1.SetSubmissionAccessRequest,
) (*adminv1.SetSubmissionAccessResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	err := h.service.SetSubmissionAccess(ctx, token, r.Organization, r.Username, r.Enabled)
	return rpcResult(&adminv1.SetSubmissionAccessResponse{Ok: err == nil}, err)
}

func (s *rpcService) PutUser(ctx context.Context, r *adminv1.PutUserRequest) (*adminv1.PutUserResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	value, err := h.service.PutUser(ctx, token, r.Username, r.Password, r.Enabled)
	return rpcResult(&adminv1.PutUserResponse{Id: value}, err)
}

func (s *rpcService) ReplaceGrants(
	ctx context.Context,
	r *adminv1.ReplaceGrantsRequest,
) (*adminv1.ReplaceGrantsResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	err := h.service.ReplaceGrants(ctx, token, r.User, mapValues(r.Grants, fromGrant))
	return rpcResult(&adminv1.ReplaceGrantsResponse{Ok: err == nil}, err)
}

func (s *rpcService) SetEnabled(
	ctx context.Context,
	r *adminv1.SetEnabledRequest,
) (*adminv1.SetEnabledResponse, error) {
	h := s.h
	token := h.rpcToken(ctx)
	err := h.service.SetEnabled(ctx, token, r.User, r.Enabled)
	return rpcResult(&adminv1.SetEnabledResponse{Ok: err == nil}, err)
}
