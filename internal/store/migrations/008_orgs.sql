-- v0.2: organizations, teams and repository roles.
--
-- Every repository belongs to an organization. A person's role on a
-- repository is the highest of: a direct grant, a grant to one of their
-- teams, the organization's default role for members, and admin for the
-- organization's owners and admins. Site admins are admin everywhere.
--
-- The data migration at the end changes nobody's access: every existing
-- person becomes an owner of every organization that existing repositories
-- were put in, and every existing person becomes a site admin. An admin
-- should prune afterwards (see docs/deploy.md, "Upgrading to v0.2").

create table orgs (
    id                uuid primary key default gen_random_uuid(),
    handle            text not null unique,                -- "acme", lowercase like a person's handle
    display_name      text not null,
    default_repo_role text not null default 'read' check (default_repo_role in ('none', 'read', 'write')),
    personal          boolean not null default false,      -- a person's own organization, named like them
    created_at        timestamptz not null default now()
);

create table org_members (
    org_id     uuid not null references orgs on delete cascade,
    person_id  uuid not null references principals on delete cascade,
    role       text not null check (role in ('owner', 'admin', 'member')),
    created_at timestamptz not null default now(),
    primary key (org_id, person_id)
);
create index org_members_person on org_members (person_id);

create table teams (
    id         uuid primary key default gen_random_uuid(),
    org_id     uuid not null references orgs on delete cascade,
    name       text not null,                              -- lowercase, unique in the org
    created_at timestamptz not null default now(),
    unique (org_id, name),
    unique (id, org_id)
);

-- Team members are always members of the team's organization: removing a
-- person from the organization removes them from its teams.
create table team_members (
    team_id    uuid not null,
    org_id     uuid not null,
    person_id  uuid not null,
    created_at timestamptz not null default now(),
    primary key (team_id, person_id),
    foreign key (team_id, org_id) references teams (id, org_id) on delete cascade,
    foreign key (org_id, person_id) references org_members (org_id, person_id) on delete cascade
);
create index team_members_person on team_members (person_id);

-- A direct grant of a role on a repository to a person (or service
-- principal) or to a team.
create table repo_grants (
    id         uuid primary key default gen_random_uuid(),
    repo_id    uuid not null references repos on delete cascade,
    person_id  uuid references principals on delete cascade,
    team_id    uuid references teams on delete cascade,
    role       text not null check (role in ('read', 'write', 'admin')),
    created_at timestamptz not null default now(),
    check ((person_id is null) <> (team_id is null))
);
create unique index repo_grants_repo_person on repo_grants (repo_id, person_id) where person_id is not null;
create unique index repo_grants_repo_team on repo_grants (repo_id, team_id) where team_id is not null;
create index repo_grants_person on repo_grants (person_id) where person_id is not null;
create index repo_grants_team on repo_grants (team_id) where team_id is not null;

alter table principals add column site_admin boolean not null default false;
alter table repos add column org_id uuid references orgs;

-- ---------- data migration ----------

-- One organization per distinct owner part of existing repository names,
-- lowercased; repositories without an owner part go to "default".
insert into orgs (handle, display_name)
select distinct h, h from (
    select case when position('/' in name) > 0 then lower(split_part(name, '/', 1)) else 'default' end as h
    from repos
) owners;

update repos r set org_id = o.id
from orgs o
where o.handle = case when position('/' in r.name) > 0 then lower(split_part(r.name, '/', 1)) else 'default' end;

-- Every existing person owns every one of those organizations.
insert into org_members (org_id, person_id, role)
select o.id, p.id, 'owner'
from orgs o cross join principals p
where p.kind = 'human';

-- Every person has a personal organization named like them. An
-- organization created above with a person's name becomes theirs.
update orgs set personal = true where handle in (select handle from principals where kind = 'human');
insert into orgs (handle, display_name, default_repo_role, personal)
select handle, display_name, 'none', true from principals where kind = 'human'
on conflict (handle) do nothing;
insert into org_members (org_id, person_id, role)
select o.id, p.id, 'owner'
from orgs o join principals p on p.handle = o.handle and p.kind = 'human'
on conflict do nothing;

-- Every existing person is a site admin, so nobody loses access today.
update principals set site_admin = true where kind = 'human';

alter table repos alter column org_id set not null;
create index repos_org on repos (org_id);
