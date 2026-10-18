-- name: ListAdminUsers :many
SELECT id,COALESCE(username,subject) AS name,CASE WHEN username IS NULL THEN 'oidc' ELSE 'local' END AS method,enabled FROM fry_admin_user ORDER BY id LIMIT 1000;

-- name: HasEnabledOperator :one
SELECT EXISTS(SELECT 1 FROM fry_admin_user u JOIN fry_admin_grant g ON g.user_id=u.id WHERE u.enabled AND g.scope='*');

-- name: SetAdminUserEnabled :execrows
UPDATE fry_admin_user SET enabled=sqlc.arg(enabled),version=version+1 WHERE id=sqlc.arg(id);

-- name: RequireSynchronousCommit :execrows
SET LOCAL synchronous_commit = 'on';

-- name: CreateAudit :execrows
INSERT INTO fry_admin_audit(actor,action,organization,target) VALUES(sqlc.arg(actor),sqlc.arg(action),sqlc.arg(organization),sqlc.arg(target));

-- name: HasAdminUsers :one
SELECT EXISTS(SELECT 1 FROM fry_admin_user);

-- name: CreateBootstrapUser :execrows
INSERT INTO fry_admin_user(id,username,password_hash) VALUES(sqlc.arg(id),sqlc.arg(username),sqlc.arg(password_hash));

-- name: GrantBootstrapAccess :execrows
INSERT INTO fry_admin_grant(user_id,scope,source,content) VALUES(sqlc.arg(user_id),'*','local',sqlc.arg(content));

-- name: ListAdminGrants :many
SELECT scope,content,source FROM fry_admin_grant WHERE user_id=sqlc.arg(user_id) ORDER BY scope,source;

-- name: GetSessionIdentity :one
SELECT u.id,COALESCE(u.username,u.subject) AS name,s.csrf,s.expires_at FROM fry_admin_session s JOIN fry_admin_user u ON u.id=s.user_id CROSS JOIN fry_admin_policy p WHERE s.token_hash=sqlc.arg(token_hash) AND u.enabled AND u.version=s.user_version AND s.expires_at>clock_timestamp() AND (s.method='local' OR s.oidc_generation=p.oidc_generation);

-- name: PruneAdminSessions :execrows
DELETE FROM fry_admin_session WHERE fry_admin_session.user_id=sqlc.arg(user_id) AND (expires_at<=clock_timestamp() OR token_hash IN (SELECT token_hash FROM fry_admin_session WHERE fry_admin_session.user_id=sqlc.arg(user_id) ORDER BY created_at DESC OFFSET 15));

-- name: CreateAdminSession :execrows
INSERT INTO fry_admin_session(token_hash,user_id,user_version,method,oidc_generation,csrf,expires_at) SELECT sqlc.arg(token_hash),id,version,sqlc.arg(method),sqlc.arg(oidc_generation),sqlc.arg(csrf),clock_timestamp()+sqlc.arg(lifetime_seconds)::bigint*interval '1 second' FROM fry_admin_user WHERE id=sqlc.arg(id) AND enabled;

-- name: GetLocalLogin :one
SELECT id,password_hash,version FROM fry_admin_user WHERE username=sqlc.arg(username) AND enabled;

-- name: ValidateLocalLogin :one
SELECT EXISTS(SELECT 1 FROM fry_admin_user WHERE id=sqlc.arg(id) AND enabled AND version=sqlc.arg(version) AND password_hash=sqlc.arg(password_hash));

-- name: DeleteAdminSession :execrows
DELETE FROM fry_admin_session WHERE token_hash=sqlc.arg(token_hash);

-- name: UpsertLocalAdminUser :one
INSERT INTO fry_admin_user(id,username,password_hash,enabled) VALUES(sqlc.arg(id),sqlc.arg(username),sqlc.arg(password_hash),sqlc.arg(enabled)) ON CONFLICT(username) DO UPDATE SET password_hash=EXCLUDED.password_hash,enabled=EXCLUDED.enabled,version=fry_admin_user.version+1 RETURNING id;

-- name: AdminUserExists :one
SELECT EXISTS(SELECT 1 FROM fry_admin_user WHERE id=sqlc.arg(id));

-- name: OrganizationExistsForGrant :one
SELECT EXISTS(SELECT 1 FROM fry_organization WHERE id=sqlc.arg(id));

-- name: DeleteLocalGrants :execrows
DELETE FROM fry_admin_grant WHERE user_id=sqlc.arg(user_id) AND source='local';

-- name: CreateLocalGrant :execrows
INSERT INTO fry_admin_grant(user_id,scope,source,content) VALUES(sqlc.arg(user_id),sqlc.arg(scope),'local',sqlc.arg(content));

-- name: ListOrganizations :many
SELECT id,name FROM fry_organization WHERE sqlc.arg(global_access)::boolean OR id IN (SELECT scope FROM fry_admin_grant WHERE user_id=sqlc.arg(user_id)) ORDER BY name,id LIMIT 1000;

-- name: UpdateOrganization :execrows
UPDATE fry_organization SET name=sqlc.arg(name) WHERE id=sqlc.arg(id);

-- name: UpsertOrganization :execrows
INSERT INTO fry_organization(id,name) VALUES(sqlc.arg(id),sqlc.arg(name)) ON CONFLICT(id) DO UPDATE SET name=EXCLUDED.name;

-- name: RegisterConfiguredOrganizations :execrows
INSERT INTO fry_organization SELECT DISTINCT organization,organization FROM fry_inbox ON CONFLICT DO NOTHING;

-- name: SetMailAccountEnabled :execrows
UPDATE fry_mail_account SET enabled=sqlc.arg(enabled),version=version+1 WHERE username=sqlc.arg(username) AND organization=sqlc.arg(organization);

-- name: ListDomains :many
SELECT name,organization,COALESCE(catch_all,'') AS catch_all,enabled FROM fry_domain WHERE organization=sqlc.arg(organization) ORDER BY name LIMIT 1000;

-- name: ListAliases :many
SELECT a.address,d.organization,ARRAY(SELECT inbox FROM fry_alias_target WHERE address=a.address ORDER BY inbox)::text[] AS inboxes,a.enabled FROM fry_alias a JOIN fry_domain d ON d.name=a.domain WHERE d.organization=sqlc.arg(organization) ORDER BY a.address LIMIT 1000;

-- name: ListMailAccounts :many
SELECT username,organization,inbox,enabled FROM fry_mail_account WHERE organization=sqlc.arg(organization) ORDER BY username LIMIT 1000;

-- name: ListInboxes :many
SELECT i.id,i.organization,(SELECT count(*) FROM fry_mail m JOIN fry_folder f ON f.id=m.folder WHERE f.inbox=i.id) AS messages FROM fry_inbox i WHERE organization=sqlc.arg(organization) ORDER BY id LIMIT 1000;

-- name: OrganizationExistsForInbox :one
SELECT EXISTS(SELECT 1 FROM fry_organization WHERE id=sqlc.arg(id));

-- name: ListMessages :many
SELECT id,raw_size,received_at,lifecycle FROM fry_receipt WHERE organization=sqlc.arg(organization) AND id>sqlc.arg(id) ORDER BY id LIMIT 100;

-- name: SetAuditOutcome :execrows
UPDATE fry_admin_audit SET outcome=sqlc.arg(outcome) WHERE id=sqlc.arg(id);

-- name: GetReceiptSize :one
SELECT raw_size FROM fry_receipt WHERE organization=sqlc.arg(organization) AND id=sqlc.arg(id);

-- name: AuthorizeContentDownload :one
INSERT INTO fry_admin_audit(actor,action,organization,target,outcome) VALUES(sqlc.arg(actor),'content-download',sqlc.arg(organization),sqlc.arg(target),'authorized') RETURNING id;

-- name: MarkContentUnavailable :execrows
UPDATE fry_admin_audit SET outcome='unavailable' WHERE id=sqlc.arg(id);

-- name: ListAudits :many
SELECT id,occurred_at,actor,action,organization,target,outcome FROM fry_admin_audit WHERE (sqlc.arg(global_access)::boolean OR organization IN (SELECT scope FROM fry_admin_grant WHERE user_id=sqlc.arg(user_id))) AND (sqlc.arg(before_id)::bigint=0 OR id<sqlc.arg(before_id)) ORDER BY id DESC LIMIT 100;

-- name: GetOIDCFingerprint :one
SELECT oidc_fingerprint FROM fry_admin_policy WHERE singleton;

-- name: UpdateOIDCFingerprint :execrows
UPDATE fry_admin_policy SET oidc_generation=oidc_generation+1,oidc_fingerprint=sqlc.arg(oidc_fingerprint) WHERE singleton;

-- name: DeleteAllOIDCGrants :execrows
DELETE FROM fry_admin_grant WHERE source='oidc';

-- name: GetConfiguredOIDCGeneration :one
SELECT oidc_generation FROM fry_admin_policy WHERE singleton;

-- name: GetLoginOIDCGeneration :one
SELECT oidc_generation FROM fry_admin_policy WHERE singleton;

-- name: DeleteExpiredOIDCLogins :execrows
DELETE FROM fry_admin_login WHERE expires_at<=clock_timestamp();

-- name: CreateOIDCLogin :execrows
INSERT INTO fry_admin_login(state_hash,browser_hash,nonce,verifier,generation,expires_at) VALUES(sqlc.arg(state_hash),sqlc.arg(browser_hash),sqlc.arg(nonce),sqlc.arg(verifier),sqlc.arg(generation),clock_timestamp()+sqlc.arg(lifetime_seconds)::bigint*interval '1 second');

-- name: ConsumeOIDCLogin :one
DELETE FROM fry_admin_login WHERE state_hash=sqlc.arg(state_hash) AND browser_hash=sqlc.arg(browser_hash) AND expires_at>clock_timestamp() RETURNING nonce,verifier,generation;

-- name: GetProvisionOIDCGeneration :one
SELECT oidc_generation FROM fry_admin_policy WHERE singleton;

-- name: UpsertOIDCUser :one
INSERT INTO fry_admin_user(id,issuer,subject) VALUES(sqlc.arg(id),sqlc.arg(issuer),sqlc.arg(subject)) ON CONFLICT(issuer,subject) DO UPDATE SET subject=EXCLUDED.subject RETURNING id,enabled;

-- name: DeleteUserOIDCGrants :execrows
DELETE FROM fry_admin_grant WHERE user_id=sqlc.arg(user_id) AND source='oidc';

-- name: OrganizationExistsForOIDCGrant :one
SELECT EXISTS(SELECT 1 FROM fry_organization WHERE id=sqlc.arg(id));

-- name: CreateOIDCGrant :execrows
INSERT INTO fry_admin_grant(user_id,scope,source,content) VALUES(sqlc.arg(user_id),sqlc.arg(scope),'oidc',sqlc.arg(content));

-- name: DeleteUserSessions :execrows
DELETE FROM fry_admin_session WHERE user_id=sqlc.arg(user_id);

-- name: GetDisabledOIDCFingerprint :one
SELECT oidc_fingerprint FROM fry_admin_policy WHERE singleton;

-- name: ClearOIDCFingerprint :execrows
UPDATE fry_admin_policy SET oidc_generation=oidc_generation+1,oidc_fingerprint='' WHERE singleton;

-- name: DeleteDisabledOIDCGrants :execrows
DELETE FROM fry_admin_grant WHERE source='oidc';

-- name: OrganizationExistsForSender :one
SELECT EXISTS(SELECT 1 FROM fry_organization WHERE id=sqlc.arg(id));

-- name: OrganizationExistsForStorage :one
SELECT EXISTS(SELECT 1 FROM fry_organization WHERE id=sqlc.arg(id));

-- name: GetStorageQuota :one
SELECT limit_bytes,version,sampled_bytes,sampled_count,sampled_at,sampled_version,active FROM fry_admin_storage_quota WHERE organization=sqlc.arg(organization);

-- name: GetMeasurementTime :one
SELECT transaction_timestamp()::timestamptz AS measured_at;

-- name: GetLatestStorageAlert :one
SELECT occurred_at,state FROM fry_admin_storage_alert WHERE organization=sqlc.arg(organization) ORDER BY id DESC LIMIT 1;

-- name: UpsertStorageQuota :execrows
INSERT INTO fry_admin_storage_quota(organization,limit_bytes) VALUES(sqlc.arg(organization),sqlc.arg(limit_bytes)) ON CONFLICT(organization) DO UPDATE SET limit_bytes=EXCLUDED.limit_bytes,version=fry_admin_storage_quota.version+1,next_evaluation=clock_timestamp();

-- name: RequireQuotaSynchronousCommit :execrows
SET LOCAL synchronous_commit='on';

-- name: CreateStorageAlert :execrows
INSERT INTO fry_admin_storage_alert(organization,policy_version,state,retained_bytes,limit_bytes) VALUES(sqlc.arg(organization),sqlc.arg(policy_version),sqlc.arg(state),sqlc.arg(retained_bytes),sqlc.arg(limit_bytes));

-- name: RecordStorageQuotaSample :execrows
UPDATE fry_admin_storage_quota SET sampled_bytes=sqlc.arg(sampled_bytes),sampled_count=sqlc.arg(sampled_count),sampled_at=clock_timestamp(),sampled_version=version,active=sqlc.arg(active),next_evaluation=CASE WHEN limit_bytes IS NULL THEN NULL ELSE clock_timestamp()+interval '30 seconds' END WHERE organization=sqlc.arg(organization);

-- name: StorageQuotaDueIndexExists :one
SELECT EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE c.relnamespace=current_schema()::regnamespace AND c.relname='fry_admin_storage_quota_due' AND i.indisvalid AND i.indisready);

-- name: ListSubmissionAccounts :many
SELECT username,organization,enabled,version,authorized_senders,authorized_domains FROM fry_submission_account WHERE organization=sqlc.arg(organization) ORDER BY username LIMIT 1000;

-- name: OrganizationExistsForSubmission :one
SELECT EXISTS(SELECT 1 FROM fry_organization WHERE id=sqlc.arg(id));

-- name: LockPolicyShared :exec
SELECT singleton FROM fry_admin_policy WHERE singleton FOR SHARE;

-- name: LockPolicyExclusive :exec
SELECT singleton FROM fry_admin_policy WHERE singleton FOR UPDATE;

-- name: ClaimNextStorageQuota :one
SELECT organization,limit_bytes,version,active FROM fry_admin_storage_quota WHERE next_evaluation<=statement_timestamp() ORDER BY next_evaluation,organization FOR UPDATE SKIP LOCKED LIMIT 1;

-- name: CheckStorageQuotaPolicySchema :many
SELECT organization,limit_bytes,version,sampled_bytes,sampled_count,sampled_at,sampled_version,active,next_evaluation FROM fry_admin_storage_quota LIMIT 0;

-- name: CheckStorageQuotaAlertSchema :many
SELECT id,organization,policy_version,occurred_at,state,retained_bytes,limit_bytes FROM fry_admin_storage_alert LIMIT 0;

-- name: RegisterSubmissionOrganization :execrows
INSERT INTO fry_organization(id,name) VALUES(sqlc.arg(organization),sqlc.arg(organization)) ON CONFLICT DO NOTHING;
