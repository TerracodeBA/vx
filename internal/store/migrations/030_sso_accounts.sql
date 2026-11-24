-- v0.5: single sign-on through an organization's identity provider (OIDC),
-- and merging duplicate accounts.

-- An organization's identity provider. The client secret is sealed with
-- the server key (VX_SECRET_KEY), never stored in plaintext.
create table org_sso (
  org_id            uuid primary key references orgs(id) on delete cascade,
  issuer            text not null,                 -- e.g. https://acme.okta.com
  client_id         text not null,
  client_secret_enc bytea not null,
  allowed_domains   text[] not null default '{}',  -- lowercased email domains; empty: any verified email
  require_sso       boolean not null default false,
  updated_by        uuid references principals(id) on delete set null,
  updated_at        timestamptz not null default now()
);

-- People linked to an identity at an OIDC provider, by the provider's
-- stable subject; one identity per person and issuer.
create table person_identities (
  issuer       text not null,
  subject      text not null,
  person_id    uuid not null references principals(id) on delete cascade,
  email        text not null default '',
  created_at   timestamptz not null default now(),
  last_used_at timestamptz,
  primary key (issuer, subject),
  unique (person_id, issuer)
);

-- Which organizations' single sign-on a token was issued through. A web
-- session gets a row when its person signs in through the organization's
-- provider; tokens made from such a token (CLI browser login, agent
-- tokens, vx ui) inherit its rows.
create table token_sso (
  token_id    uuid not null references tokens(id) on delete cascade,
  org_id      uuid not null references orgs(id) on delete cascade,
  verified_at timestamptz not null default now(),
  primary key (token_id, org_id)
);
create index token_sso_org on token_sso (org_id);

-- Nonces of ID tokens already used, so a captured sign-in cannot be
-- replayed while its token is still valid.
create table oidc_nonces (
  nonce_hash bytea primary key,  -- sha256 of issuer, client id and nonce
  expires_at timestamptz not null
);

-- The web session that approved a CLI browser login, so the CLI's token
-- inherits its single sign-on.
alter table cli_logins add column approved_token_id uuid references tokens(id) on delete set null;

-- Merged accounts stay (history names them) but cannot be used.
alter table principals
  add column disabled_at timestamptz,
  add column merged_into uuid references principals(id);
