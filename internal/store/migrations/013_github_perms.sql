-- Roles on GitHub-backed repositories come from GitHub (see
-- internal/service/github_roles.go). github_role_cache (migration 011)
-- holds repository roles; this adds the organization memberships GitHub
-- reported, cached the same way, and the indexes webhooks delete by.

-- A person's membership in a GitHub organization (the account of an
-- installation), as GitHub reported it: '' (not a member), member or
-- admin. Keyed by the GitHub account id, so it survives reinstalling the
-- App.
create table github_org_membership_cache (
  github_org_id bigint not null,
  principal_id uuid not null references principals(id) on delete cascade,
  role text not null check (role in ('', 'member', 'admin')),
  checked_at timestamptz not null,
  primary key (github_org_id, principal_id)
);

create index github_org_membership_cache_principal on github_org_membership_cache (principal_id);
create index github_role_cache_principal on github_role_cache (principal_id);
create index repos_github_installation on repos (github_installation_id) where github_installation_id is not null;
