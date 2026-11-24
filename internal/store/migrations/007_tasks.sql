-- Tasks: an effort's shared plan. A task is open (nobody on it), taken (an
-- owner works on it) or done. "Blocked" is never stored: a task reads as
-- blocked while one of the tasks it depends on is not done.
create table tasks (
    id         uuid primary key,
    seq        bigint generated always as identity, -- stable order between equal created_at
    repo_id    uuid not null references repos on delete cascade,
    effort_id  uuid not null references efforts on delete cascade,
    title      text not null check (length(title) between 1 and 200 and strpos(title, E'\n') = 0),
    body       text not null default '' check (length(body) <= 4000),
    state      text not null default 'open' check (state in ('open', 'taken', 'done')),
    owner_id   uuid references principals,
    created_by uuid not null references principals,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now(),
    check (state <> 'open' or owner_id is null),
    check (state <> 'taken' or owner_id is not null)
);
create index tasks_effort_order on tasks (effort_id, created_at, seq);
create index tasks_owner on tasks (owner_id) where owner_id is not null;

-- task_deps says task_id waits for depends_on_id. Both are in the same
-- effort; the service checks that and refuses cycles.
create table task_deps (
    task_id       uuid not null references tasks on delete cascade,
    depends_on_id uuid not null references tasks on delete cascade,
    primary key (task_id, depends_on_id),
    check (task_id <> depends_on_id)
);
create index task_deps_depends_on on task_deps (depends_on_id);
