-- The check work queue, runner kinds and CI tokens (#47, part 2).

-- A job is one check to run on one tree of an effort by someone other than
-- the landing agent: an agent of some kind, CI or a person. Runners take
-- the oldest queued job they may run with a lease; an expired lease puts it
-- back with the attempt counted, and after max_attempts it fails.
create table check_jobs (
    id                uuid primary key,
    repo_id           uuid not null references repos on delete cascade,
    effort_id         uuid not null references efforts on delete cascade,
    check_name        text not null,
    runner            text not null,                -- agent:KIND, ci or human
    reporter          text not null default '',     -- the check's reporter rule, for picking jobs a runner may report
    allow_same_person boolean not null default false,
    trigger           text not null check (trigger in ('land', 'ship', 'manual')),
    change_id         text not null default '',     -- the change it was made for; '' for a whole effort (ship)
    changeset_hash    bytea,                        -- the changeset whose tree it checks; null before it landed (a land gate)
    intent            text not null default '',     -- the change's intent
    base_tree         bytea,                        -- what the diff input is against
    tree              bytea not null,               -- the tree the result must be for
    author_principal  text not null default '',     -- who made the change (ref), for the trust rules
    author_person     text not null default '',
    inputs            text[] not null default '{}', -- the check's declared inputs
    "values"          jsonb not null default '{}',  -- named input values (preview_url, build.preview_url, ...)
    state             text not null default 'queued'
                      check (state in ('queued', 'leased', 'done', 'failed', 'superseded')),
    attempts          integer not null default 0,   -- leases that expired
    max_attempts      integer not null default 3 check (max_attempts > 0),
    timeout_seconds   integer not null default 1800 check (timeout_seconds > 0),
    lease_owner       uuid references principals on delete set null,
    lease_expires_at  timestamptz,
    leased_at         timestamptz,
    reason            text not null default '',     -- why it failed or was superseded
    result_id         text not null default '',     -- the result that finished it
    requested_by      uuid references principals on delete set null,
    superseded_by     uuid references check_jobs on delete set null,
    created_at        timestamptz not null default now(),
    updated_at        timestamptz not null default now()
);

-- Taking the oldest queued job; finding expired leases.
create index check_jobs_queued on check_jobs (created_at, id) where state = 'queued';
create index check_jobs_leased on check_jobs (lease_expires_at) where state = 'leased';
create index check_jobs_effort on check_jobs (effort_id, check_name, created_at);
create index check_jobs_repo on check_jobs (repo_id, created_at);
-- One pending job per check and tree of an effort.
create unique index check_jobs_pending on check_jobs (effort_id, check_name, tree) where state in ('queued', 'leased');

-- Which runner kinds an agent serves (agent:browser jobs go only to agents
-- that declared browser). Set by the agent's person. No row: none.
create table agent_runner_kinds (
    agent_id   uuid primary key references principals on delete cascade,
    kinds      text[] not null default '{}',
    updated_by uuid references principals on delete set null,
    updated_at timestamptz not null default now()
);

-- A CI token: a service principal that may only report results (for the
-- named checks; none: every check) on one repository and read the inputs
-- of its jobs. It has no repository grant, so everything else is "not
-- found" to it.
create table ci_tokens (
    id           uuid primary key,
    repo_id      uuid not null references repos on delete cascade,
    name         text not null,
    principal_id uuid not null unique references principals on delete cascade,
    token_id     uuid not null references tokens on delete cascade,
    checks       text[] not null default '{}',
    created_by   uuid references principals on delete set null,
    created_at   timestamptz not null default now(),
    revoked_at   timestamptz
);
create unique index ci_tokens_name on ci_tokens (repo_id, name) where revoked_at is null;
