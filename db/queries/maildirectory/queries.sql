-- name: ResolveIncomingRoute :one
SELECT d.organization,CASE WHEN a.address IS NOT NULL THEN CASE WHEN a.enabled THEN ARRAY(SELECT inbox FROM fry_alias_target WHERE address=a.address ORDER BY inbox) ELSE ARRAY[]::text[] END ELSE CASE WHEN d.catch_all IS NULL THEN ARRAY[]::text[] ELSE ARRAY[d.catch_all] END END::text[] AS inboxes FROM fry_domain d LEFT JOIN fry_alias a ON a.domain=d.name AND a.address=sqlc.arg(address) WHERE d.name=sqlc.arg(name) AND d.enabled;

-- name: GetMailAccountCredentials :one
SELECT username,organization,inbox,version,password_hash FROM fry_mail_account WHERE username=sqlc.arg(username) AND enabled;

-- name: ValidateMailAccount :one
SELECT EXISTS(SELECT 1 FROM fry_mail_account WHERE username=sqlc.arg(username) AND organization=sqlc.arg(organization) AND inbox=sqlc.arg(inbox) AND version=sqlc.arg(version) AND enabled);

-- name: InboxBelongsToOrganization :one
SELECT EXISTS(SELECT 1 FROM fry_inbox WHERE id=sqlc.arg(id) AND organization=sqlc.arg(organization));

-- name: InsertDomainIfAbsent :execrows
INSERT INTO fry_domain(name,organization,catch_all,enabled) VALUES(sqlc.arg(name),sqlc.arg(organization),sqlc.arg(catch_all),sqlc.arg(enabled)) ON CONFLICT DO NOTHING;

-- name: LockDomainOrganization :one
SELECT organization FROM fry_domain WHERE name=sqlc.arg(name) FOR UPDATE;

-- name: UpdateDomain :execrows
UPDATE fry_domain SET catch_all=sqlc.arg(catch_all),enabled=sqlc.arg(enabled) WHERE name=sqlc.arg(name);

-- name: LockAliasDomainOrganization :one
SELECT organization FROM fry_domain WHERE name=sqlc.arg(name) FOR UPDATE;

-- name: InsertAliasIfAbsent :execrows
INSERT INTO fry_alias(address,domain,enabled) VALUES(sqlc.arg(address),sqlc.arg(domain),sqlc.arg(enabled)) ON CONFLICT DO NOTHING;

-- name: UpdateAlias :execrows
UPDATE fry_alias SET enabled=sqlc.arg(enabled) WHERE address=sqlc.arg(address);

-- name: DeleteAliasTargets :execrows
DELETE FROM fry_alias_target WHERE address=sqlc.arg(address);

-- name: CreateAliasTarget :execrows
INSERT INTO fry_alias_target(address,inbox) VALUES(sqlc.arg(address),sqlc.arg(inbox));

-- name: InsertMailAccountIfAbsent :execrows
INSERT INTO fry_mail_account(username,organization,inbox,password_hash,enabled) VALUES(sqlc.arg(username),sqlc.arg(organization),sqlc.arg(inbox),sqlc.arg(password_hash),sqlc.arg(enabled)) ON CONFLICT DO NOTHING;

-- name: LockMailAccountOrganization :one
SELECT organization FROM fry_mail_account WHERE username=sqlc.arg(username) FOR UPDATE;

-- name: UpdateMailAccount :execrows
UPDATE fry_mail_account SET inbox=sqlc.arg(inbox),password_hash=sqlc.arg(password_hash),enabled=sqlc.arg(enabled),version=version+1 WHERE username=sqlc.arg(username);

-- name: RequireDirectorySynchronousCommit :execrows
SET LOCAL synchronous_commit = 'on';

-- name: GetSubmissionCredentials :one
SELECT username,organization,version,password_hash,authorized_senders,authorized_domains FROM fry_submission_account WHERE username=sqlc.arg(username) AND enabled;

-- name: ValidateSubmissionAccount :one
SELECT EXISTS(SELECT 1 FROM fry_submission_account WHERE username=sqlc.arg(username) AND organization=sqlc.arg(organization) AND version=sqlc.arg(version) AND enabled);

-- name: RotateSubmissionPassword :execrows
UPDATE fry_submission_account SET password_hash=sqlc.arg(password_hash),version=version+1 WHERE username=sqlc.arg(username) AND organization=sqlc.arg(organization);

-- name: SetSubmissionAccountEnabled :execrows
UPDATE fry_submission_account SET enabled=sqlc.arg(enabled),version=version+1 WHERE username=sqlc.arg(username) AND organization=sqlc.arg(organization);

-- name: RequireSubmissionSynchronousCommit :execrows
SET LOCAL synchronous_commit='on';

-- name: CheckSubmissionPolicySchema :many
SELECT authorized_senders,authorized_domains FROM fry_submission_account LIMIT 0;

-- name: LockSeedSubmissionUsername :execrows
SELECT pg_advisory_xact_lock(hashtextextended('fry/submission/' || sqlc.arg(username)::text,0));

-- name: LockSubmissionOrganization :one
SELECT organization FROM fry_submission_account WHERE username=sqlc.arg(username) FOR UPDATE;

-- name: InsertSeedSubmissionAccount :execrows
INSERT INTO fry_submission_account(username,organization,password_hash,enabled,authorized_senders,authorized_domains) VALUES(sqlc.arg(username),sqlc.arg(organization),sqlc.arg(password_hash),sqlc.arg(enabled),sqlc.arg(authorized_senders),sqlc.arg(authorized_domains));

-- name: LockNewSubmissionUsername :execrows
SELECT pg_advisory_xact_lock(hashtextextended('fry/submission/' || sqlc.arg(username)::text,0));

-- name: InsertSubmissionAccountIfAbsent :execrows
INSERT INTO fry_submission_account(username,organization,password_hash,enabled,authorized_senders,authorized_domains) VALUES(sqlc.arg(username),sqlc.arg(organization),sqlc.arg(password_hash),sqlc.arg(enabled),sqlc.arg(authorized_senders),sqlc.arg(authorized_domains)) ON CONFLICT(username) DO NOTHING;

-- name: UpdateSubmissionPolicy :execrows
UPDATE fry_submission_account SET authorized_senders=sqlc.arg(authorized_senders),authorized_domains=sqlc.arg(authorized_domains),version=version+1 WHERE username=sqlc.arg(username) AND organization=sqlc.arg(organization);
