-- Shipments: effort changes turned into a branch and a pull request.
create table shipments (
    id           uuid primary key default gen_random_uuid(),
    effort_id    uuid not null references efforts on delete cascade,
    title        text not null,
    branch       text not null check (branch like 'vx/%'),
    target       text not null,
    base_git_sha text not null,
    head_git_sha text not null,
    provider_ref text not null default '',   -- PR number on GitHub
    url          text not null default '',
    state        text not null default 'open' check (state in ('open', 'draft', 'merged', 'closed', 'pushed')),
    created_by   uuid not null references principals,
    created_at   timestamptz not null default now(),
    updated_at   timestamptz not null default now(),
    unique (effort_id, branch)
);

create table shipment_changes (
    shipment_id    uuid not null references shipments on delete cascade,
    position       int not null,
    changeset_hash bytea not null,
    change_id      text not null,
    git_sha        text not null,
    primary key (shipment_id, position)
);
