-- Experimental schema v1. Apply only to a fresh, dedicated prototype schema.
CREATE TABLE fry_profile (
 singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
 epoch bigint NOT NULL CHECK (epoch > 0),
 required_copies integer NOT NULL CHECK (required_copies = 2)
);
INSERT INTO fry_profile VALUES (true,1,2);
CREATE TABLE fry_ingest_catalog (
 singleton boolean PRIMARY KEY CHECK(singleton),
 version integer NOT NULL CHECK(version=1),
 algorithm text NOT NULL CHECK(algorithm='attempt-sha256-v1'),
 catalog jsonb NOT NULL
);
CREATE TABLE fry_ingest_shard (
 id text PRIMARY KEY CHECK(id ~ '^[0-9a-f]{32}$'),
 member_a text NOT NULL UNIQUE,
 member_b text NOT NULL UNIQUE,
 CHECK(member_a<member_b)
);
CREATE TABLE fry_attempt (
 placement_version integer NOT NULL DEFAULT 1 CHECK(placement_version=1),
 ingest_shard text NOT NULL REFERENCES fry_ingest_shard(id),
 id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
 receipt_id text NOT NULL UNIQUE CHECK (receipt_id ~ '^[0-9a-f]{32}$'),
 organization text NOT NULL CHECK (organization ~ '^[0-9a-f]{32}$'),
 envelope jsonb NOT NULL,
 state text NOT NULL CHECK (state IN ('preparing','accepted','aborted')),
 epoch bigint NOT NULL CHECK (epoch > 0),
 profile_epoch bigint NOT NULL,
 lease_until timestamptz NOT NULL,
 recovery_seconds bigint NOT NULL CHECK (recovery_seconds > 0)
);
CREATE TABLE fry_target (
 attempt text NOT NULL REFERENCES fry_attempt(id),
 member text NOT NULL,
 incarnation text NOT NULL,
 failure_domain text NOT NULL,
 owner_epoch bigint NOT NULL DEFAULT 0 CHECK(owner_epoch>=0),
 locator jsonb,
 cancel_confirmed boolean NOT NULL DEFAULT false,
 PRIMARY KEY(attempt,member),
 UNIQUE(attempt,incarnation),
 UNIQUE(attempt,failure_domain)
);
CREATE TABLE fry_receipt (
 id text PRIMARY KEY REFERENCES fry_attempt(receipt_id),
 attempt text NOT NULL UNIQUE REFERENCES fry_attempt(id),
 organization text NOT NULL,
 received_at timestamptz NOT NULL,
 raw_size bigint NOT NULL CHECK (raw_size >= 0),
 sha256 bytea NOT NULL CHECK (octet_length(sha256)=32),
 profile_epoch bigint NOT NULL,
 protection text NOT NULL CHECK (protection='two-domain')
);
CREATE TABLE fry_destination (
 receipt text NOT NULL REFERENCES fry_receipt(id),
 inbox text NOT NULL,
 recipients jsonb NOT NULL,
 PRIMARY KEY(receipt,inbox)
);
CREATE TABLE fry_reference (
 receipt text NOT NULL REFERENCES fry_receipt(id),
 kind text NOT NULL CHECK (kind IN ('recovery','pending-mailbox')),
 reference_key text NOT NULL,
 expires_at timestamptz,
 PRIMARY KEY(receipt,kind,reference_key)
);
CREATE TABLE fry_work (
 organization text NOT NULL CHECK(organization ~ '^[0-9a-f]{32}$'),
 received_at timestamptz NOT NULL,
 receipt text NOT NULL REFERENCES fry_receipt(id),
 inbox text NOT NULL,
 kind text NOT NULL CHECK (kind='publish-mailbox'),
 PRIMARY KEY(receipt,inbox,kind)
);
-- One source-message identity, not a highest-UID cursor that could skip gaps.
CREATE TABLE fry_source (
 connection text NOT NULL,
 folder text NOT NULL,
 uidvalidity bigint NOT NULL CHECK (uidvalidity > 0),
 uid bigint NOT NULL CHECK (uid > 0),
 attempt text NOT NULL UNIQUE REFERENCES fry_attempt(id),
 accepted boolean NOT NULL DEFAULT false,
 PRIMARY KEY(connection,folder,uidvalidity,uid)
);
CREATE TABLE fry_cleanup (
 attempt text NOT NULL,
 member text NOT NULL,
 cancel_confirmed boolean NOT NULL DEFAULT false,
 bytes_removed boolean NOT NULL DEFAULT false,
 PRIMARY KEY(attempt,member),
 FOREIGN KEY(attempt,member) REFERENCES fry_target(attempt,member)
);

-- Pack jobs are single upload attempts with unique, never-reused object keys.
ALTER TABLE fry_receipt ADD COLUMN manifest_version bigint NOT NULL DEFAULT 1;
ALTER TABLE fry_receipt ADD COLUMN pack_id text;
CREATE TABLE fry_pack (
 id text PRIMARY KEY,
 organization text NOT NULL,
 object_key text NOT NULL UNIQUE,
 state text NOT NULL CHECK (state IN ('building','published','retiring','retired')),
 epoch bigint NOT NULL DEFAULT 1,
 lease_until timestamptz NOT NULL,
 image_size bigint,
 image_sha256 bytea CHECK (image_sha256 IS NULL OR octet_length(image_sha256)=32),
 etag text
);
ALTER TABLE fry_receipt ADD FOREIGN KEY(pack_id) REFERENCES fry_pack(id);
CREATE TABLE fry_pack_member (
 pack text NOT NULL REFERENCES fry_pack(id),
 receipt text NOT NULL REFERENCES fry_receipt(id),
 manifest_version bigint NOT NULL,
 locator jsonb,
 PRIMARY KEY(pack,receipt)
);

ALTER TABLE fry_receipt ADD COLUMN lifecycle text NOT NULL DEFAULT 'live' CHECK (lifecycle IN ('live','retiring','removed'));
ALTER TABLE fry_receipt ADD COLUMN eligible_at timestamptz;
ALTER TABLE fry_receipt ADD COLUMN cleanup_due timestamptz;
ALTER TABLE fry_target ADD COLUMN bytes_removed boolean NOT NULL DEFAULT false;
ALTER TABLE fry_reference DROP CONSTRAINT fry_reference_kind_check;
ALTER TABLE fry_reference ADD CHECK (kind IN ('recovery','pending-mailbox','mailbox','delivery'));
-- Released reference identities are retained: a delayed retry cannot recreate them.
CREATE TABLE fry_reference_release (
 receipt text NOT NULL REFERENCES fry_receipt(id),
 kind text NOT NULL,
 reference_key text NOT NULL,
 PRIMARY KEY(receipt,kind,reference_key)
);
-- An expired deadline is NOT proof that the owner's actual read handle closed.
CREATE TABLE fry_read_pin (
 id text PRIMARY KEY,
 receipt text NOT NULL REFERENCES fry_receipt(id),
 pack text REFERENCES fry_pack(id),
 deadline timestamptz NOT NULL
);
CREATE INDEX fry_read_pin_pack ON fry_read_pin(pack);
CREATE INDEX fry_read_pin_receipt ON fry_read_pin(receipt);
CREATE INDEX fry_receipt_cleanup ON fry_receipt(cleanup_due,id) WHERE lifecycle='retiring';
ALTER TABLE fry_pack ADD COLUMN source_pack text REFERENCES fry_pack(id);

-- Authoritative local file selection and crash-recoverable rewrite obligations.
CREATE TABLE fry_member (
 id text PRIMARY KEY,
 incarnation text NOT NULL,
 failure_domain text NOT NULL,
 directory text NOT NULL
);
-- Process ownership is distinct from the byte-file incarnation. An advisory
-- lock becoming available does not prove a prior process stopped disk I/O.
CREATE TABLE fry_member_owner (
 member text NOT NULL REFERENCES fry_member(id),
 epoch bigint NOT NULL CHECK(epoch>0),
 instance text NOT NULL UNIQUE,
 closed boolean NOT NULL DEFAULT false,
 PRIMARY KEY(member,epoch)
);
ALTER TABLE fry_read_pin ADD COLUMN owner_member text;
ALTER TABLE fry_read_pin ADD COLUMN owner_epoch bigint;
ALTER TABLE fry_read_pin ADD COLUMN incarnation text;
ALTER TABLE fry_read_pin ADD FOREIGN KEY(owner_member,owner_epoch) REFERENCES fry_member_owner(member,epoch);
ALTER TABLE fry_read_pin ADD CHECK ((owner_member IS NULL AND owner_epoch IS NULL AND incarnation IS NULL) OR (owner_member IS NOT NULL AND owner_epoch IS NOT NULL AND incarnation IS NOT NULL));
CREATE TABLE fry_member_read (
 id text PRIMARY KEY,
 member text NOT NULL,
 owner_epoch bigint NOT NULL,
 receipt text NOT NULL REFERENCES fry_receipt(id),
 parent_pin text NOT NULL,
 incarnation text NOT NULL,
 closed boolean NOT NULL DEFAULT false,
 FOREIGN KEY(member,owner_epoch) REFERENCES fry_member_owner(member,epoch)
);
CREATE TABLE fry_hot_rewrite (
 id text PRIMARY KEY,
 member text NOT NULL REFERENCES fry_member(id),
 old_incarnation text NOT NULL,
 old_directory text NOT NULL,
 new_directory text NOT NULL,
 kind text NOT NULL DEFAULT 'rewrite' CHECK(kind IN ('rewrite','rotate','sealed-reclaim')),
 state text NOT NULL CHECK(state IN ('building','published','removed','abandoned','complete'))
);

ALTER TABLE fry_hot_rewrite ADD COLUMN new_incarnation text;
ALTER TABLE fry_hot_rewrite ADD COLUMN kept_records bigint;
ALTER TABLE fry_hot_rewrite ADD CHECK ((kind='sealed-reclaim' AND kept_records IS NOT NULL AND kept_records>=0) OR (kind<>'sealed-reclaim' AND kept_records IS NULL));
CREATE TABLE fry_member_file (
 member text NOT NULL REFERENCES fry_member(id),
 file_id text NOT NULL UNIQUE,
 directory text NOT NULL,
 state text NOT NULL CHECK(state IN ('building','current','sealed','retiring','removed')),
 sealed_records bigint CHECK(sealed_records>=1),
 rewrite_job text REFERENCES fry_hot_rewrite(id),
 PRIMARY KEY(member,file_id),
 UNIQUE(member,directory),
 CHECK ((state='current' AND rewrite_job IS NULL) OR (state<>'current' AND rewrite_job IS NOT NULL))
);
CREATE INDEX fry_member_file_sealed ON fry_member_file(member,file_id) WHERE state='sealed';
CREATE UNIQUE INDEX fry_member_file_current ON fry_member_file(member) WHERE state='current';
CREATE INDEX fry_member_file_active ON fry_member_file(member) WHERE state IN ('building','retiring');
CREATE TABLE fry_member_profile (
 member text PRIMARY KEY REFERENCES fry_member(id),
 enabled boolean NOT NULL,
 max_message_bytes bigint NOT NULL CHECK(max_message_bytes>0),
 max_concurrent_appends bigint NOT NULL CHECK(max_concurrent_appends>0),
 max_bytes bigint NOT NULL CHECK(max_bytes>=64),
 max_records bigint NOT NULL CHECK(max_records>=0),
 max_file_bytes bigint NOT NULL CHECK(max_file_bytes>=0),
 staging_bytes bigint NOT NULL CHECK(staging_bytes>=0),
 lane_version smallint NOT NULL CHECK(lane_version=1),
 lane_count smallint NOT NULL CHECK(lane_count>=0 AND lane_count<=16),
 CHECK((enabled AND max_records>0 AND max_file_bytes>0 AND staging_bytes>0 AND lane_count>0) OR (NOT enabled AND max_records=0 AND max_file_bytes=0 AND staging_bytes=0 AND lane_count=0))
);
ALTER TABLE fry_member_file ADD COLUMN charged_bytes bigint NOT NULL DEFAULT 0 CHECK(charged_bytes>=0);
ALTER TABLE fry_member_file ADD CHECK(state<>'sealed' OR (sealed_records IS NOT NULL AND charged_bytes>=64));
ALTER TABLE fry_hot_rewrite ADD COLUMN reserved_bytes bigint NOT NULL DEFAULT 0 CHECK(reserved_bytes>=0);
CREATE INDEX fry_member_file_charge ON fry_member_file(member) INCLUDE(charged_bytes) WHERE state<>'removed';
CREATE INDEX fry_hot_rewrite_charge ON fry_hot_rewrite(member) INCLUDE(reserved_bytes) WHERE state='building';
ALTER TABLE fry_member_file ADD CHECK(state<>'removed' OR charged_bytes=0);
ALTER TABLE fry_hot_rewrite ADD CHECK(state='building' OR reserved_bytes=0);
CREATE TABLE fry_member_file_lane (
 member text NOT NULL,
 file_id text NOT NULL,
 lane smallint NOT NULL CHECK(lane>=0 AND lane<16),
 record_ceiling bigint NOT NULL CHECK(record_ceiling>0),
 byte_ceiling bigint NOT NULL CHECK(byte_ceiling>0),
 reserved_records bigint NOT NULL DEFAULT 0 CHECK(reserved_records>=0 AND reserved_records<=record_ceiling),
 reserved_bytes bigint NOT NULL DEFAULT 0 CHECK(reserved_bytes>=0 AND reserved_bytes<=byte_ceiling),
 PRIMARY KEY(member,file_id,lane),
 FOREIGN KEY(member,file_id) REFERENCES fry_member_file(member,file_id)
);
ALTER TABLE fry_target ADD COLUMN capacity_lane smallint;
ALTER TABLE fry_target ADD FOREIGN KEY(member,incarnation,capacity_lane) REFERENCES fry_member_file_lane(member,file_id,lane);
ALTER TABLE fry_target ADD COLUMN capacity_reserved_bytes bigint CHECK(capacity_reserved_bytes>0);
ALTER TABLE fry_target ADD COLUMN capacity_settled boolean NOT NULL DEFAULT false;
ALTER TABLE fry_target ADD CHECK(NOT capacity_settled OR capacity_reserved_bytes IS NOT NULL);
ALTER TABLE fry_target ADD CHECK((capacity_lane IS NULL)=(capacity_reserved_bytes IS NULL));


ALTER TABLE fry_pack ADD COLUMN staging_closed boolean NOT NULL DEFAULT false;

ALTER TABLE fry_pack ADD COLUMN staging_owner text;
ALTER TABLE fry_receipt ADD COLUMN removed_at timestamptz;

-- Conventional mailbox state is distinct from immutable receipt identity.
CREATE SEQUENCE fry_uidvalidity MINVALUE 1 MAXVALUE 4294967295 NO CYCLE;
CREATE TABLE fry_inbox (
 id text PRIMARY KEY,
 organization text NOT NULL
);
CREATE TABLE fry_folder (
 id text PRIMARY KEY,
 inbox text NOT NULL REFERENCES fry_inbox(id),
 name text NOT NULL,
 uidvalidity bigint NOT NULL DEFAULT nextval('fry_uidvalidity'),
 uidnext bigint NOT NULL DEFAULT 1 CHECK(uidnext BETWEEN 1 AND 4294967296),
 keywords text[] NOT NULL DEFAULT '{}' CHECK(cardinality(keywords)<=256),
 UNIQUE(inbox,name)
);
-- Subscriptions belong to names, including names whose folders were deleted.
CREATE TABLE fry_subscription (
 inbox text NOT NULL REFERENCES fry_inbox(id),
 name text NOT NULL,
 PRIMARY KEY(inbox,name)
);
-- Retain each name's last UID namespace even after deletion or rename.
CREATE TABLE fry_folder_name (
 inbox text NOT NULL REFERENCES fry_inbox(id),
 name text NOT NULL,
 folder text NOT NULL,
 uidvalidity bigint NOT NULL CHECK(uidvalidity BETWEEN 1 AND 4294967295),
 PRIMARY KEY(inbox,name)
);
CREATE TABLE fry_mail (
 id text PRIMARY KEY,
 folder text NOT NULL REFERENCES fry_folder(id),
 uid bigint NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
 receipt text NOT NULL REFERENCES fry_receipt(id),
 flags text[] NOT NULL DEFAULT '{}' CHECK(cardinality(flags)<=128),
 internal_date timestamptz NOT NULL,
 UNIQUE(folder,uid)
);
CREATE INDEX fry_mail_receipt ON fry_mail(receipt);

-- Explicit placement for collected/imported messages; keep pending work even
-- when its destination is unavailable, rather than losing accepted originals.
ALTER TABLE fry_work ADD COLUMN folder text;
ALTER TABLE fry_work ADD COLUMN flags text[] NOT NULL DEFAULT '{}' CHECK(cardinality(flags)<=128);
ALTER TABLE fry_work ADD COLUMN internal_date timestamptz;
CREATE TABLE fry_source_connection (
 id text PRIMARY KEY,
 organization text NOT NULL,
 fingerprint bytea NOT NULL CHECK(octet_length(fingerprint)=32)
);
ALTER TABLE fry_work ADD COLUMN available_at timestamptz NOT NULL DEFAULT clock_timestamp();
ALTER TABLE fry_work ADD COLUMN failures bigint NOT NULL DEFAULT 0;
ALTER TABLE fry_work ADD COLUMN last_error text;
CREATE INDEX fry_work_available ON fry_work(available_at);
CREATE INDEX fry_work_organization_available ON fry_work(organization,available_at,received_at,receipt,inbox);

CREATE TABLE fry_outbound (
 id text PRIMARY KEY,
 receipt text NOT NULL REFERENCES fry_receipt(id),
 relay text NOT NULL,
 recipient text NOT NULL,
 state text NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','handed-off','failed')),
 deadline timestamptz NOT NULL,
 available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 lease_until timestamptz,
 epoch bigint NOT NULL DEFAULT 0,
 attempts bigint NOT NULL DEFAULT 0,
 response_code integer,
 diagnostic text,
 completed_at timestamptz,
 UNIQUE(receipt,recipient)
);
CREATE INDEX fry_outbound_due ON fry_outbound(available_at) WHERE state='queued';
CREATE TABLE fry_failure_notice (
 id text PRIMARY KEY,
 job text NOT NULL UNIQUE REFERENCES fry_outbound(id),
 organization text NOT NULL,
 relay text NOT NULL,
 recipient text NOT NULL,
 failed_recipient text NOT NULL,
 response_code integer NOT NULL,
 diagnostic text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','queued')),
 receipt text REFERENCES fry_receipt(id)
);
ALTER TABLE fry_failure_notice ADD COLUMN rendering_host text;
ALTER TABLE fry_failure_notice ADD COLUMN notice_retry_seconds bigint;

-- APPEND's original acknowledgment survives subsequent moves/expunges. Folder
-- identity is deliberately not a foreign key: deleting a folder must not erase it.
CREATE TABLE fry_append_result (
 receipt text PRIMARY KEY REFERENCES fry_receipt(id),
 folder text NOT NULL,
 uidvalidity bigint NOT NULL CHECK(uidvalidity BETWEEN 1 AND 4294967295),
 uid bigint NOT NULL CHECK(uid BETWEEN 1 AND 4294967295)
);

-- Bounded reconciliation scans only unresolved process-owned reads/aborts.
CREATE INDEX fry_member_read_unclosed ON fry_member_read(id) WHERE NOT closed;
CREATE INDEX fry_attempt_aborted ON fry_attempt(id) WHERE state='aborted';

-- History migration owns a fresh, inaccessible inbox until explicit handoff.
CREATE TABLE fry_migration (
 id text PRIMARY KEY,
 organization text NOT NULL,
 inbox text NOT NULL UNIQUE REFERENCES fry_inbox(id),
 fingerprint bytea NOT NULL CHECK(octet_length(fingerprint)=32),
 generation bigint NOT NULL DEFAULT 0,
 phase text NOT NULL DEFAULT 'copying' CHECK(phase IN ('copying','final','ready','complete')),
 freeze_attestation jsonb,
 deadline timestamptz,
 inventory jsonb NOT NULL DEFAULT '[]',
 manifest bytea,
 completed_at timestamptz
);
ALTER TABLE fry_inbox ADD COLUMN migration_job text REFERENCES fry_migration(id);
CREATE TABLE fry_migration_folder (
 job text NOT NULL REFERENCES fry_migration(id),
 source_name text NOT NULL,
 destination text NOT NULL,
 folder text NOT NULL REFERENCES fry_folder(id),
 uidvalidity bigint NOT NULL CHECK(uidvalidity BETWEEN 1 AND 4294967295),
 upper_uid bigint NOT NULL CHECK(upper_uid BETWEEN 0 AND 4294967295),
 next_uid bigint NOT NULL DEFAULT 1 CHECK(next_uid BETWEEN 1 AND 4294967296),
 subscribed boolean NOT NULL,
 PRIMARY KEY(job,source_name), UNIQUE(job,destination)
);
CREATE TABLE fry_migration_entry (
 id text PRIMARY KEY,
 job text NOT NULL REFERENCES fry_migration(id),
 source_name text NOT NULL,
 uidvalidity bigint NOT NULL,
 uid bigint NOT NULL CHECK(uid BETWEEN 1 AND 4294967295),
 seen_generation bigint NOT NULL,
 flags text[] NOT NULL,
 internal_date timestamptz NOT NULL,
 raw_size bigint NOT NULL CHECK(raw_size>=0),
 attempt text,
 mail text UNIQUE,
 UNIQUE(job,source_name,uidvalidity,uid)
);
CREATE INDEX fry_migration_entry_seen ON fry_migration_entry(job,seen_generation,id) WHERE mail IS NOT NULL;
CREATE TABLE fry_migration_copy (
 attempt text PRIMARY KEY,
 job text NOT NULL REFERENCES fry_migration(id),
 entry text NOT NULL REFERENCES fry_migration_entry(id),
 generation bigint NOT NULL,
 envelope jsonb NOT NULL,
 receipt text REFERENCES fry_receipt(id)
);
CREATE INDEX fry_migration_copy_pending ON fry_migration_copy(job,generation,attempt) WHERE receipt IS NULL;
CREATE TABLE fry_migration_event (
 job text NOT NULL REFERENCES fry_migration(id),
 generation bigint NOT NULL,
 kind text NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 detail jsonb NOT NULL
);

CREATE INDEX fry_destination_inbox ON fry_destination(inbox);

-- Bounded lifecycle scheduling: organization turns and unresolved pack lookups.
CREATE INDEX fry_receipt_pack_schedule ON fry_receipt(organization,received_at,id) WHERE lifecycle='live' AND pack_id IS NULL;
CREATE INDEX fry_receipt_pack_oversized ON fry_receipt(raw_size,organization) WHERE lifecycle='live' AND pack_id IS NULL;
CREATE INDEX fry_pack_schedule ON fry_pack(organization,id) WHERE state IN ('building','published','retiring');
CREATE INDEX fry_pack_rewrite_pending ON fry_pack(source_pack) WHERE state IN ('building','retiring');
CREATE INDEX fry_pack_member_receipt ON fry_pack_member(receipt,pack);

-- Member maintenance gates survive coordinator disconnects and uncertain RPCs.
ALTER TABLE fry_member ADD COLUMN maintenance_job text;
CREATE TABLE fry_member_maintenance (
 selected_file text,
 id text PRIMARY KEY,
 member text NOT NULL REFERENCES fry_member(id),
 owner_epoch bigint NOT NULL,
 expected_incarnation text NOT NULL,
 kind text NOT NULL DEFAULT 'rewrite' CHECK(kind IN ('rewrite','rotate','sealed-reclaim','auto-rotate')),
 state text NOT NULL CHECK(state IN ('running','complete','failed')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 completed_at timestamptz,
 FOREIGN KEY(member,selected_file) REFERENCES fry_member_file(member,file_id),
 CHECK(kind='sealed-reclaim' OR selected_file IS NULL)
);

-- Cleanup deadline follows the accepting policy, including on remote reclaimers.
ALTER TABLE fry_attempt ADD COLUMN cleanup_microseconds bigint NOT NULL DEFAULT 86400000000 CHECK(cleanup_microseconds>=1000000);

-- Physical object address is immutable for all historical pack obligations.
CREATE TABLE fry_object_namespace (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 namespace text CHECK(namespace IS NULL OR length(namespace) BETWEEN 1 AND 4096)
);
INSERT INTO fry_object_namespace(singleton) VALUES(true);

-- Relay IDs retain routing/account identity for all accepted outbound history.
-- Passwords and TLS trust material are deliberately excluded.
CREATE TABLE fry_relay_binding (
 id text PRIMARY KEY CHECK(id ~ '^[0-9a-f]{32}$'),
 address text NOT NULL,
 transport text NOT NULL CHECK(transport IN ('tls','starttls','loopback')),
 username text NOT NULL
);
CREATE INDEX fry_outbound_relay_history ON fry_outbound(relay);
CREATE INDEX fry_failure_notice_relay_history ON fry_failure_notice(relay);

-- Outgoing sender authority is independent of incoming domain ownership.
CREATE TABLE fry_sender_grant (
 id text PRIMARY KEY CHECK(id ~ '^[0-9a-f]{32}$'),
 organization text NOT NULL,
 scope text NOT NULL,
 version bigint NOT NULL DEFAULT 1 CHECK(version>0),
 active boolean NOT NULL DEFAULT false,
 challenge_epoch bigint NOT NULL DEFAULT 0 CHECK(challenge_epoch>=0),
 evidence text NOT NULL DEFAULT '',
 actor text NOT NULL DEFAULT '',
 reason text NOT NULL DEFAULT '',
 verified_at timestamptz,
 UNIQUE(organization,scope)
);
CREATE TABLE fry_sender_challenge (
 id text PRIMARY KEY CHECK(id ~ '^[0-9a-f]{32}$'),
 grant_id text NOT NULL REFERENCES fry_sender_grant(id),
 epoch bigint NOT NULL CHECK(epoch>0),
 token_hash bytea NOT NULL CHECK(octet_length(token_hash)=32),
 expires_at timestamptz NOT NULL,
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','completed','superseded')),
 completed_by text,
 UNIQUE(grant_id,epoch)
);
CREATE INDEX fry_sender_challenge_pending ON fry_sender_challenge(grant_id) WHERE state='pending';
CREATE TABLE fry_sender_grant_event (
 grant_id text NOT NULL REFERENCES fry_sender_grant(id),
 version bigint NOT NULL,
 action text NOT NULL CHECK(action IN ('enable','revoke')),
 actor text NOT NULL,
 evidence text NOT NULL,
 reason text NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(grant_id,version)
);

-- Derived raw headers remain governed by their receipt's content lifetime.
CREATE TABLE fry_header_metadata (
 receipt text PRIMARY KEY REFERENCES fry_receipt(id),
 raw_header bytea NOT NULL CHECK(octet_length(raw_header) BETWEEN 1 AND 1048576)
);

-- Credentials belong to the receipt authority so revocation fences finalization.
CREATE TABLE fry_submission_account (
 username text PRIMARY KEY CHECK(length(username) BETWEEN 1 AND 128),
 organization text NOT NULL CHECK(organization ~ '^[0-9a-f]{32}$'),
 password_hash text NOT NULL,
 enabled boolean NOT NULL,
 authorized_senders text[] NOT NULL,
 authorized_domains text[] NOT NULL,
 CHECK(cardinality(authorized_senders)+cardinality(authorized_domains) BETWEEN 1 AND 128),
 version bigint NOT NULL DEFAULT 1 CHECK(version > 0)
);

-- Exact logical retained originals; not replica/pack/filesystem consumption.
-- This marker is initialized only with a fresh receipt schema, never backfilled
-- or inferred from historical rows during runtime startup.
CREATE TABLE fry_storage_accounting (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 version integer NOT NULL CHECK(version=1),
 algorithm text NOT NULL CHECK(algorithm='receipt-sha256-64-v1')
);
INSERT INTO fry_storage_accounting(singleton,version,algorithm) VALUES(true,1,'receipt-sha256-64-v1');
CREATE TABLE fry_storage_usage (
 organization text NOT NULL CHECK(organization ~ '^[0-9a-f]{32}$'),
 slot smallint NOT NULL CHECK(slot BETWEEN 0 AND 63),
 retained_originals bigint NOT NULL CHECK(retained_originals>=0),
 retained_bytes bigint NOT NULL CHECK(retained_bytes>=0),
 CHECK(retained_originals>0 OR retained_bytes=0),
 PRIMARY KEY(organization,slot)
);
