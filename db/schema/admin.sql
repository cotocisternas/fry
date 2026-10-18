-- Applied with the fresh prototype receipt schema, never as an implicit upgrade.
CREATE TABLE fry_admin_policy (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 oidc_generation bigint NOT NULL DEFAULT 1,
 oidc_fingerprint text NOT NULL DEFAULT ''
);
INSERT INTO fry_admin_policy(singleton) VALUES(true);
CREATE TABLE fry_admin_user (
 id text PRIMARY KEY,
 username text UNIQUE,
 password_hash text,
 issuer text,
 subject text,
 enabled boolean NOT NULL DEFAULT true,
 version bigint NOT NULL DEFAULT 1,
 UNIQUE(issuer,subject),
 CHECK ((username IS NOT NULL AND password_hash IS NOT NULL AND issuer IS NULL AND subject IS NULL)
     OR (username IS NULL AND password_hash IS NULL AND issuer IS NOT NULL AND subject IS NOT NULL))
);
CREATE TABLE fry_admin_grant (
 user_id text NOT NULL REFERENCES fry_admin_user(id),
 scope text NOT NULL, -- '*' means global operator; other values are organization IDs
 source text NOT NULL CHECK(source IN ('local','oidc')),
 content boolean NOT NULL DEFAULT false,
 PRIMARY KEY(user_id,scope,source)
);
CREATE TABLE fry_admin_session (
 token_hash bytea PRIMARY KEY CHECK(octet_length(token_hash)=32),
 user_id text NOT NULL REFERENCES fry_admin_user(id),
 user_version bigint NOT NULL,
 method text NOT NULL CHECK(method IN ('local','oidc')),
 oidc_generation bigint,
 csrf text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL
);
CREATE INDEX fry_admin_session_user ON fry_admin_session(user_id);
CREATE TABLE fry_organization (
 id text PRIMARY KEY CHECK(id ~ '^[0-9a-f]{32}$'),
 name text NOT NULL
);
CREATE TABLE fry_admin_audit (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 occurred_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 actor text NOT NULL,
 action text NOT NULL,
 organization text NOT NULL DEFAULT '',
 target text NOT NULL DEFAULT '',
 outcome text NOT NULL DEFAULT 'committed'
);
CREATE TABLE fry_admin_login (
 state_hash bytea PRIMARY KEY,
 browser_hash bytea NOT NULL,
 nonce text NOT NULL,
 verifier text NOT NULL,
 generation bigint NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL
);

-- Advisory policies do not participate in receipt admission or physical capacity.
CREATE TABLE fry_admin_storage_quota (
 organization text PRIMARY KEY REFERENCES fry_organization(id),
 limit_bytes bigint CHECK(limit_bytes>0),
 version bigint NOT NULL DEFAULT 1 CHECK(version>0),
 sampled_bytes bigint CHECK(sampled_bytes>=0),
 sampled_count bigint CHECK(sampled_count>=0),
 sampled_at timestamptz,
 sampled_version bigint,
 active boolean NOT NULL DEFAULT false,
 next_evaluation timestamptz DEFAULT clock_timestamp()
);
CREATE INDEX fry_admin_storage_quota_due ON fry_admin_storage_quota(next_evaluation,organization) WHERE next_evaluation IS NOT NULL;
CREATE TABLE fry_admin_storage_alert (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 organization text NOT NULL REFERENCES fry_organization(id),
 policy_version bigint NOT NULL,
 occurred_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 state text NOT NULL CHECK(state IN ('active','resolved')),
 retained_bytes bigint NOT NULL CHECK(retained_bytes>=0),
 limit_bytes bigint CHECK(limit_bytes>0)
);
CREATE INDEX fry_admin_storage_alert_latest ON fry_admin_storage_alert(organization,id DESC);
