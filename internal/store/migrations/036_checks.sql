-- Company-defined checks (#47): results of the checks a repository's
-- vx.toml ([[check]]) or its organization defines, bound to a change and
-- the exact tree they were gathered on. A new tree needs new evidence.
-- Logs and artifacts are blobs of the repository (recorded in
-- repo_objects as they are written), so the organization's stored-bytes
-- quota counts them and the purge of a deleted organization deletes them.
create table check_results (
    id           uuid primary key default gen_random_uuid(),
    repo_id      uuid not null references repos on delete cascade,
    effort_id    uuid references efforts on delete cascade,
    change_id    text not null,
    tree         bytea not null,                 -- the tree the result describes
    check_name   text not null,                  -- '' for a vx land --run command no check names
    status       text not null check (status in ('pass', 'fail', 'neutral', 'pending')),
    summary      text not null default '',
    log_hash     bytea,                          -- blob of the log, when one was sent
    log_size     bigint not null default 0,
    data         jsonb,                          -- structured data, validated against the check's outputs
    data_name    text not null default '',       -- the name it was sent as, e.g. steps.json
    reporter_id  uuid references principals on delete set null,
    reporter     text not null,                  -- principal ref, e.g. agent:alice/claude-review
    on_behalf_of text not null default '',
    model        text not null default '',
    ci_token_id  uuid,                           -- the CI service token that reported it, if any
    how          text not null default '',       -- the command of a run check
    source       text not null check (source in ('report', 'land', 'review')),
    started_at   timestamptz,
    finished_at  timestamptz,
    created_at   timestamptz not null default now()
);
create index check_results_tree on check_results (repo_id, tree, check_name, created_at desc);
create index check_results_change on check_results (repo_id, change_id, created_at desc);
create index check_results_effort on check_results (effort_id, created_at desc);

create table check_artifacts (
    result_id    uuid not null references check_results on delete cascade,
    position     int not null,
    name         text not null,
    content_type text not null,
    hash         bytea not null,
    size         bigint not null,
    primary key (result_id, position),
    unique (result_id, name)
);
