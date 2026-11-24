-- vx schema, phase 1: principals, repos, upstream branches, efforts,
-- workspaces, changesets and the operation log.

create table principals (
    id           uuid primary key default gen_random_uuid(),
    kind         text not null check (kind in ('human', 'agent', 'service')),
    handle       text not null unique,          -- "alice", "alice/claude-backend"
    parent_id    uuid references principals,    -- the human an agent acts for
    display_name text not null,
    email        text not null default '',
    scopes       jsonb not null default '[]',   -- reserved for phase 2
    created_at   timestamptz not null default now(),
    check ((kind = 'agent') = (parent_id is not null))
);

create table tokens (
    id           uuid primary key default gen_random_uuid(),
    principal_id uuid not null references principals on delete cascade,
    secret_hash  bytea not null unique,         -- sha256 of the token
    name         text not null default '',
    created_at   timestamptz not null default now(),
    last_used_at timestamptz,
    revoked_at   timestamptz
);

create table repos (
    id         uuid primary key default gen_random_uuid(),
    name       text not null unique,            -- "acme/shop-api"
    created_at timestamptz not null default now()
);

create table upstreams (
    repo_id        uuid primary key references repos on delete cascade,
    provider       text not null check (provider in ('github', 'git', 'none')),
    remote         text not null default '',    -- owner/name for github, URL for git
    default_branch text not null,
    created_at     timestamptz not null default now()
);

create table changesets (
    repo_id          uuid not null references repos on delete cascade,
    hash             bytea not null,
    change_id        text not null,
    origin           text not null check (origin in ('vx', 'upstream')),
    git_sha          text,
    effort_id        uuid,
    first_parent     bytea,
    tree             bytea not null,
    author_principal text not null default '',
    title            text not null,
    raw              bytea not null,            -- canonical encoding; hash covers it
    created_at       timestamptz not null,
    primary key (repo_id, hash)
);
create index changesets_change_id on changesets (repo_id, change_id);
create index changesets_git_sha on changesets (repo_id, git_sha) where git_sha is not null;
create index changesets_effort on changesets (effort_id, created_at);

create table file_changes (
    repo_id        uuid not null,
    changeset_hash bytea not null,
    path           text not null,
    op             text not null check (op in ('add', 'modify', 'delete')),
    old_hash       bytea,
    new_hash       bytea,
    mode           text not null default 'file',
    added          int not null default 0,
    deleted        int not null default 0,
    primary key (repo_id, changeset_hash, path),
    foreign key (repo_id, changeset_hash) references changesets on delete cascade
);
create index file_changes_path on file_changes (repo_id, path);

create table tracked_branches (
    repo_id   uuid not null references repos on delete cascade,
    name      text not null,
    head      bytea not null,                   -- changeset hash
    git_sha   text not null default '',
    synced_at timestamptz not null default now(),
    primary key (repo_id, name)
);

create table efforts (
    id          uuid primary key default gen_random_uuid(),
    repo_id     uuid not null references repos on delete cascade,
    slug        text not null,
    name        text not null,
    goal        text not null default '',
    base_branch text not null,
    base        bytea not null,                 -- upstream changeset the effort sits on
    head        bytea not null,
    version     bigint not null default 0,      -- for compare-and-swap on head
    state       text not null default 'open'
                check (state in ('open', 'shipping', 'merged', 'abandoned')),
    owner_id    uuid not null references principals,
    created_at  timestamptz not null default now(),
    updated_at  timestamptz not null default now(),
    unique (repo_id, slug)
);

alter table changesets
    add constraint changesets_effort_fk foreign key (effort_id) references efforts on delete set null;

create table workspaces (
    id         uuid primary key default gen_random_uuid(),
    effort_id  uuid not null references efforts on delete cascade,
    owner_id   uuid not null references principals,
    intent     text not null,
    change_id  text not null,                   -- stable across rebases of this work
    base       bytea not null,
    state      text not null default 'open' check (state in ('open', 'landed', 'abandoned')),
    landed_as  bytea,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);
create index workspaces_effort on workspaces (effort_id, state);

create table op_log (
    id       bigserial primary key,
    repo_id  uuid references repos on delete cascade,
    actor_id uuid references principals,
    op       text not null,
    target   text not null default '',
    before   jsonb,
    after    jsonb,
    at       timestamptz not null default now()
);
create index op_log_repo on op_log (repo_id, id);
