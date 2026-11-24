-- Superadmins (#33). principals.site_admin now means superadmin: the
-- person runs the server (people, invites, sign-up, the GitHub App, server
-- settings, operation log metadata) and has no role on any repository.
-- A repository's admins can grant a superadmin support access: a role on
-- that repository until a time, recorded in the operation log.

create table support_grants (
  repo_id    uuid not null references repos(id) on delete cascade,
  person_id  uuid not null references principals(id) on delete cascade,
  role       text not null check (role in ('read', 'write', 'admin')),
  expires_at timestamptz not null,
  reason     text not null default '',
  granted_by uuid references principals(id) on delete set null,
  created_at timestamptz not null default now(),
  primary key (repo_id, person_id)
);
create index support_grants_person on support_grants (person_id);

-- One-time notices about an upgrade: the server logs each once
-- (logged_at), and the admin panel shows it until a superadmin marks it
-- reviewed (resolved_at).
create table server_notices (
  key         text primary key,
  created_at  timestamptz not null default now(),
  logged_at   timestamptz,
  resolved_at timestamptz,
  resolved_by uuid references principals(id) on delete set null
);

-- A server that already has site admins is an upgrade: until now they were
-- admin on every repository, and from now on they are not.
insert into server_notices (key)
select 'superadmin-review' where exists (select 1 from principals where site_admin and kind = 'human');
