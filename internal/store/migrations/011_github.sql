-- GitHub as identity and permission source (contract for the sign-in effort).
-- Secrets are stored encrypted with the server key (VX_SECRET_KEY); columns
-- ending in _enc hold AES-GCM ciphertext, never plaintext.

alter table principals
  add column github_user_id bigint unique,
  add column github_login text;

alter table orgs
  add column github_org_id bigint unique,
  add column github_login text;

alter table repos
  add column github_repo_id bigint unique,
  add column github_installation_id bigint;

-- The vx GitHub App of this server, created through the manifest flow.
create table github_app (
  id int primary key default 1 check (id = 1),
  app_id bigint not null,
  slug text not null,
  html_url text not null,
  client_id text not null,
  client_secret_enc bytea not null,
  private_key_enc bytea not null,
  webhook_secret_enc bytea not null,
  created_by uuid references principals(id) on delete set null,
  created_at timestamptz not null default now()
);

-- Installations of the App on GitHub organizations or user accounts.
create table github_installations (
  id bigint primary key,
  account_id bigint not null,
  account_login text not null,
  account_type text not null check (account_type in ('Organization', 'User')),
  org_id uuid references orgs(id) on delete set null,
  repository_selection text not null default 'selected',
  suspended_at timestamptz,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

-- Effective roles read from GitHub, cached briefly; webhooks delete rows.
create table github_role_cache (
  repo_id uuid not null references repos(id) on delete cascade,
  principal_id uuid not null references principals(id) on delete cascade,
  role text not null check (role in ('', 'read', 'write', 'admin')),
  checked_at timestamptz not null,
  primary key (repo_id, principal_id)
);

-- People allowed to sign up while sign-up is invite-only.
create table signup_invites (
  github_login text primary key, -- lowercased
  invited_by uuid references principals(id) on delete set null,
  note text not null default '',
  created_at timestamptz not null default now(),
  used_at timestamptz
);

-- Browser login for the CLI (device-code style).
create table cli_logins (
  device_hash bytea primary key, -- sha256 of the device code; the code itself is never stored
  user_code text not null unique, -- what the person confirms in the browser, e.g. WDJB-MJHT
  client_name text not null, -- e.g. "vx on alice-laptop"
  status text not null default 'pending' check (status in ('pending', 'approved', 'denied', 'used')),
  principal_id uuid references principals(id) on delete cascade,
  token_secret_enc bytea, -- set on approval, cleared when the CLI collects it
  expires_at timestamptz not null,
  created_at timestamptz not null default now()
);
