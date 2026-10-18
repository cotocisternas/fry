CREATE TABLE fry_domain (
 name text PRIMARY KEY,
 organization text NOT NULL REFERENCES fry_organization(id),
 catch_all text REFERENCES fry_inbox(id),
 enabled boolean NOT NULL DEFAULT true
);
CREATE INDEX fry_domain_organization ON fry_domain(organization);
CREATE TABLE fry_alias (
 address text PRIMARY KEY,
 domain text NOT NULL REFERENCES fry_domain(name),
 enabled boolean NOT NULL DEFAULT true
);
CREATE TABLE fry_alias_target (
 address text NOT NULL REFERENCES fry_alias(address),
 inbox text NOT NULL REFERENCES fry_inbox(id),
 PRIMARY KEY(address,inbox)
);
CREATE TABLE fry_mail_account (
 username text PRIMARY KEY,
 organization text NOT NULL REFERENCES fry_organization(id),
 inbox text NOT NULL REFERENCES fry_inbox(id),
 password_hash text NOT NULL,
 enabled boolean NOT NULL DEFAULT true,
 version bigint NOT NULL DEFAULT 1
);
CREATE INDEX fry_mail_account_organization ON fry_mail_account(organization);
