-- Path scopes and budgets per agent (#5). No row: no limits.

create table agent_scopes (
    agent_id           uuid primary key references principals on delete cascade,
    allow              text[] not null default '{}', -- globs; empty: every path the person may change
    deny               text[] not null default '{}', -- globs; win over allow
    max_lands_per_day  integer not null default 0 check (max_lands_per_day >= 0), -- 0: no limit
    max_lines          integer not null default 0 check (max_lines >= 0),         -- changed lines per land
    max_files          integer not null default 0 check (max_files >= 0),         -- changed files per land
    max_workspaces     integer not null default 0 check (max_workspaces >= 0),    -- open at once
    updated_by         uuid references principals on delete set null,
    updated_at         timestamptz not null default now()
);

-- Counting an agent's lands of the day.
create index changesets_author_created on changesets (author_principal, created_at) where origin = 'vx';

-- Counting an agent's open workspaces.
create index workspaces_owner_open on workspaces (owner_id) where state = 'open';
