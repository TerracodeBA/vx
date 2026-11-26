-- #47 part 3: organization checks and GitHub check runs per check.
--
-- An organization's owners and admins define [[check]] tables for every
-- repository of the organization. Each save is a new version; the newest
-- is in force, the older ones are the history. source is the TOML as
-- written, checks the parsed definitions (rules.CheckDef as JSON).
create table org_check_versions (
    org_id      uuid not null references orgs(id) on delete cascade,
    version     int not null check (version > 0),
    source      text not null,
    checks      jsonb not null default '[]',
    created_by  uuid references principals(id) on delete set null,
    created_ref text not null default '',
    created_at  timestamptz not null default now(),
    primary key (org_id, version)
);

-- The GitHub check run vx keeps for one check on one pull request head,
-- updated as results arrive. state is what was last published.
create table github_check_runs (
    shipment_id uuid not null references shipments(id) on delete cascade,
    check_name  text not null,
    head_sha    text not null,
    github_id   bigint not null,
    state       text not null,
    updated_at  timestamptz not null default now(),
    primary key (shipment_id, check_name, head_sha)
);
