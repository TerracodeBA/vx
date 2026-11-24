-- Claims: soft "I am changing these paths or symbols now" markers. They
-- never block anything. A claim is active while released_at is null and
-- expires_at is in the future; a claim tied to a workspace is released when
-- the workspace lands or is abandoned.
create table claims (
    id           uuid primary key,
    seq          bigint generated always as identity, -- insertion order, breaks ties between equal created_at
    repo_id      uuid not null references repos on delete cascade,
    effort_id    uuid not null references efforts on delete cascade,
    owner_id     uuid not null references principals,
    workspace_id uuid references workspaces on delete cascade,
    targets      text[] not null check (cardinality(targets) between 1 and 20),
    reason       text not null default '' check (length(reason) <= 300),
    created_at   timestamptz not null default now(),
    expires_at   timestamptz not null,
    released_at  timestamptz,
    check (expires_at > created_at)
);
create index claims_active on claims (effort_id, expires_at) where released_at is null;
create index claims_workspace on claims (workspace_id) where released_at is null and workspace_id is not null;
create index claims_owner on claims (owner_id) where released_at is null;
