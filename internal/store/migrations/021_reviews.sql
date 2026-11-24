-- Landing policies (#6): changes to paths under a "human" [[policy]] of
-- vx.toml wait for a person before they land. A review holds the author's
-- snapshot; the effort head does not move until someone approves it. An
-- approval lands it with the normal merge onto the current head; when that
-- conflicts the review stays pending and goes back to the author. A
-- workspace has at most one pending review: landing again from it replaces
-- the snapshot.
create table reviews (
    id           uuid primary key default gen_random_uuid(),
    repo_id      uuid not null references repos on delete cascade,
    effort_id    uuid not null references efforts on delete cascade,
    workspace_id uuid not null references workspaces on delete cascade,
    author_id    uuid not null references principals,
    change_id    text not null,
    intent       text not null,
    base         bytea not null,                 -- changeset the snapshot was made on
    tree         bytea not null,                 -- the author's snapshot
    evidence     jsonb,                          -- object.Evidence, as the land would store it
    depends_on   text[] not null default '{}',
    model        text not null default '',
    session      text not null default '',
    paths        text[] not null,                -- changed files under the human policy
    policies     text[] not null default '{}',   -- which [[policy]] tables matched, for people
    approvers    jsonb not null default '[]',    -- [[...], ...]: a person must be in every list
    state        text not null check (state in ('pending', 'approved', 'rejected', 'withdrawn')),
    conflicts    text[] not null default '{}',   -- paths that conflicted when it was last approved
    decided_by   uuid references principals,     -- approved or rejected by; also who hit the conflicts
    decided_at   timestamptz,
    reason       text not null default '',       -- why it was rejected
    landed       bytea,                          -- the changeset the approval landed
    created_at   timestamptz not null default now(),
    updated_at   timestamptz not null default now()
);
create unique index reviews_one_pending on reviews (workspace_id) where state = 'pending';
create index reviews_pending on reviews (repo_id, effort_id) where state = 'pending';
create index reviews_effort on reviews (effort_id, created_at desc);

-- System notes may be addressed to one principal (the author of a
-- reviewed change), and a note kind for approvals. r2-feedback's migration
-- of v0.4 runs the same statements, so either may run last.
alter table notes drop constraint if exists notes_check1;
alter table notes drop constraint if exists notes_to_check;
alter table notes add constraint notes_to_check check (kind = 'question' or system or to_id is null);
alter table notes drop constraint if exists notes_kind_check;
alter table notes add constraint notes_kind_check
  check (kind in ('plan', 'decision', 'interface', 'question', 'answer', 'blocker', 'handoff', 'overlap', 'review', 'ci', 'approval', 'rebase'));
