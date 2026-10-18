// Typed Connect-Web adapter for the ordinary DOM administration screens.
import { createClient, type CallOptions, Code, ConnectError } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { fromJson, toJson, type JsonValue } from "@bufbuild/protobuf";
import { AdminService,
 GetConfigRequestSchema, GetConfigResponseSchema,
 LoginRequestSchema, LoginResponseSchema,
 GetSessionRequestSchema, GetSessionResponseSchema,
 LogoutRequestSchema, LogoutResponseSchema,
 ListOrganizationsRequestSchema, ListOrganizationsResponseSchema,
 ListInboxesRequestSchema, ListInboxesResponseSchema,
 ListSenderGrantsRequestSchema, ListSenderGrantsResponseSchema,
 ListDomainsRequestSchema, ListDomainsResponseSchema,
 ListAliasesRequestSchema, ListAliasesResponseSchema,
 ListMailAccountsRequestSchema, ListMailAccountsResponseSchema,
 ListSubmissionAccountsRequestSchema, ListSubmissionAccountsResponseSchema,
 ListMessagesRequestSchema, ListMessagesResponseSchema,
 ListAuditsRequestSchema, ListAuditsResponseSchema,
 ListUsersRequestSchema, ListUsersResponseSchema,
 PutOrganizationRequestSchema, PutOrganizationResponseSchema,
 CreateInboxRequestSchema, CreateInboxResponseSchema,
 PutDomainRequestSchema, PutDomainResponseSchema,
 PutAliasRequestSchema, PutAliasResponseSchema,
 PutMailAccountRequestSchema, PutMailAccountResponseSchema,
 CreateSenderChallengeRequestSchema, CreateSenderChallengeResponseSchema,
 VerifySenderRequestSchema, VerifySenderResponseSchema,
 RevokeSenderRequestSchema, RevokeSenderResponseSchema,
 OverrideSenderRequestSchema, OverrideSenderResponseSchema,
 SetMailAccessRequestSchema, SetMailAccessResponseSchema,
 GetMemberStorageRequestSchema, GetMemberStorageResponseSchema,
 GetStorageUsageRequestSchema, GetStorageUsageResponseSchema,
 SetStorageQuotaRequestSchema, SetStorageQuotaResponseSchema,
 CreateSubmissionAccountRequestSchema, CreateSubmissionAccountResponseSchema,
 SetSubmissionPolicyRequestSchema, SetSubmissionPolicyResponseSchema,
 RotateSubmissionPasswordRequestSchema, RotateSubmissionPasswordResponseSchema,
 SetSubmissionAccessRequestSchema, SetSubmissionAccessResponseSchema,
 PutUserRequestSchema, PutUserResponseSchema,
 ReplaceGrantsRequestSchema, ReplaceGrantsResponseSchema,
 SetEnabledRequestSchema, SetEnabledResponseSchema,
} from "../../../gen/admin/ts/fry/admin/v1/admin_pb.js";

const rpc = createClient(AdminService, createConnectTransport({
 baseUrl: location.origin,
 useBinaryFormat: false,
 fetch: (input, init) => fetch(input, { ...init, credentials: "same-origin", redirect: "error" }),
}));

type Call = (input: JsonValue, options: CallOptions) => Promise<JsonValue>;
const jsonOptions = { useProtoFieldName: true, alwaysEmitImplicit: true };
const commands: Record<string, Call> = {
 "GET config": async (input, options) => toJson(GetConfigResponseSchema, await rpc.getConfig(fromJson(GetConfigRequestSchema, input), options), jsonOptions),
 "POST login": async (input, options) => toJson(LoginResponseSchema, await rpc.login(fromJson(LoginRequestSchema, input), options), jsonOptions),
 "GET session": async (input, options) => toJson(GetSessionResponseSchema, await rpc.getSession(fromJson(GetSessionRequestSchema, input), options), jsonOptions),
 "POST logout": async (input, options) => toJson(LogoutResponseSchema, await rpc.logout(fromJson(LogoutRequestSchema, input), options), jsonOptions),
 "GET organizations": async (input, options) => toJson(ListOrganizationsResponseSchema, await rpc.listOrganizations(fromJson(ListOrganizationsRequestSchema, input), options), jsonOptions),
 "GET inboxes": async (input, options) => toJson(ListInboxesResponseSchema, await rpc.listInboxes(fromJson(ListInboxesRequestSchema, input), options), jsonOptions),
 "GET sender-grants": async (input, options) => toJson(ListSenderGrantsResponseSchema, await rpc.listSenderGrants(fromJson(ListSenderGrantsRequestSchema, input), options), jsonOptions),
 "GET domains": async (input, options) => toJson(ListDomainsResponseSchema, await rpc.listDomains(fromJson(ListDomainsRequestSchema, input), options), jsonOptions),
 "GET aliases": async (input, options) => toJson(ListAliasesResponseSchema, await rpc.listAliases(fromJson(ListAliasesRequestSchema, input), options), jsonOptions),
 "GET mail-accounts": async (input, options) => toJson(ListMailAccountsResponseSchema, await rpc.listMailAccounts(fromJson(ListMailAccountsRequestSchema, input), options), jsonOptions),
 "GET submission-accounts": async (input, options) => toJson(ListSubmissionAccountsResponseSchema, await rpc.listSubmissionAccounts(fromJson(ListSubmissionAccountsRequestSchema, input), options), jsonOptions),
 "GET messages": async (input, options) => toJson(ListMessagesResponseSchema, await rpc.listMessages(fromJson(ListMessagesRequestSchema, input), options), jsonOptions),
 "GET audit": async (input, options) => toJson(ListAuditsResponseSchema, await rpc.listAudits(fromJson(ListAuditsRequestSchema, input), options), jsonOptions),
 "GET users": async (input, options) => toJson(ListUsersResponseSchema, await rpc.listUsers(fromJson(ListUsersRequestSchema, input), options), jsonOptions),
 "POST organizations": async (input, options) => toJson(PutOrganizationResponseSchema, await rpc.putOrganization(fromJson(PutOrganizationRequestSchema, input), options), jsonOptions),
 "POST inboxes": async (input, options) => toJson(CreateInboxResponseSchema, await rpc.createInbox(fromJson(CreateInboxRequestSchema, input), options), jsonOptions),
 "POST domains": async (input, options) => toJson(PutDomainResponseSchema, await rpc.putDomain(fromJson(PutDomainRequestSchema, input), options), jsonOptions),
 "POST aliases": async (input, options) => toJson(PutAliasResponseSchema, await rpc.putAlias(fromJson(PutAliasRequestSchema, input), options), jsonOptions),
 "POST mail-accounts": async (input, options) => toJson(PutMailAccountResponseSchema, await rpc.putMailAccount(fromJson(PutMailAccountRequestSchema, input), options), jsonOptions),
 "POST sender-challenge": async (input, options) => toJson(CreateSenderChallengeResponseSchema, await rpc.createSenderChallenge(fromJson(CreateSenderChallengeRequestSchema, input), options), jsonOptions),
 "POST sender-verify": async (input, options) => toJson(VerifySenderResponseSchema, await rpc.verifySender(fromJson(VerifySenderRequestSchema, input), options), jsonOptions),
 "POST sender-revoke": async (input, options) => toJson(RevokeSenderResponseSchema, await rpc.revokeSender(fromJson(RevokeSenderRequestSchema, input), options), jsonOptions),
 "POST sender-override": async (input, options) => toJson(OverrideSenderResponseSchema, await rpc.overrideSender(fromJson(OverrideSenderRequestSchema, input), options), jsonOptions),
 "POST mail-access": async (input, options) => toJson(SetMailAccessResponseSchema, await rpc.setMailAccess(fromJson(SetMailAccessRequestSchema, input), options), jsonOptions),
 "GET member-storage": async (input, options) => toJson(GetMemberStorageResponseSchema, await rpc.getMemberStorage(fromJson(GetMemberStorageRequestSchema, input), options), jsonOptions),
 "GET storage-usage": async (input, options) => toJson(GetStorageUsageResponseSchema, await rpc.getStorageUsage(fromJson(GetStorageUsageRequestSchema, input), options), jsonOptions),
 "POST storage-quota": async (input, options) => toJson(SetStorageQuotaResponseSchema, await rpc.setStorageQuota(fromJson(SetStorageQuotaRequestSchema, input), options), jsonOptions),
 "POST submission-accounts": async (input, options) => toJson(CreateSubmissionAccountResponseSchema, await rpc.createSubmissionAccount(fromJson(CreateSubmissionAccountRequestSchema, input), options), jsonOptions),
 "POST submission-policy": async (input, options) => toJson(SetSubmissionPolicyResponseSchema, await rpc.setSubmissionPolicy(fromJson(SetSubmissionPolicyRequestSchema, input), options), jsonOptions),
 "POST submission-password": async (input, options) => toJson(RotateSubmissionPasswordResponseSchema, await rpc.rotateSubmissionPassword(fromJson(RotateSubmissionPasswordRequestSchema, input), options), jsonOptions),
 "POST submission-access": async (input, options) => toJson(SetSubmissionAccessResponseSchema, await rpc.setSubmissionAccess(fromJson(SetSubmissionAccessRequestSchema, input), options), jsonOptions),
 "POST users": async (input, options) => toJson(PutUserResponseSchema, await rpc.putUser(fromJson(PutUserRequestSchema, input), options), jsonOptions),
 "POST grants": async (input, options) => toJson(ReplaceGrantsResponseSchema, await rpc.replaceGrants(fromJson(ReplaceGrantsRequestSchema, input), options), jsonOptions),
 "POST access": async (input, options) => toJson(SetEnabledResponseSchema, await rpc.setEnabled(fromJson(SetEnabledRequestSchema, input), options), jsonOptions),
};

export async function call(path: string, data: JsonValue | undefined, csrf: string): Promise<JsonValue> {
 const target = new URL(path, location.origin);
 const method = data === undefined ? "GET" : "POST";
 const command = commands[method + " " + target.pathname.slice(1)];
 if (!command) throw new Error("Unknown administration operation.");
 const input: Record<string, JsonValue> = {};
 target.searchParams.forEach((value, key) => { input[key] = key === "limit" ? Number(value) : value; });
 const response = await command(data === undefined ? input : data, {headers: {"X-FRY-CSRF": csrf}});
 if (response && typeof response === "object" && !Array.isArray(response)) {
  const result = response.result ?? response.items;
  if (result !== undefined) return nullableFields(result);
 }
 return response;
}

// Forms distinguish disabled/unsampled values from present zero/false values.
function nullableFields(value: JsonValue): JsonValue {
 if (Array.isArray(value)) return value.map(nullableFields);
 if (value === null || typeof value !== "object") return value;
 const result: Record<string, JsonValue> = {};
 for (const [key, item] of Object.entries(value)) result[key] = nullableFields(item);
 for (const key of ["limit_bytes", "sampled_at", "sampled_bytes", "sampled_originals", "sampled_policy_version", "last_transition_at", "last_transition_state"])
  if (!(key in result) && ("retained_bytes" in result || "checked_at" in result)) result[key] = null;
 return result;
}

export function unauthenticated(error: unknown): boolean {
 return error instanceof ConnectError && error.code === Code.Unauthenticated;
}
