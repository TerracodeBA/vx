-- v0.4 (#7): review comments and CI results from the git host reach the
-- agents whose changes they are about; efforts rebase themselves when their
-- base branch moves.

-- System notes may be addressed to one principal (a review comment to the
-- author of the line). The v0.3 finish effort's migration runs the same
-- statements; whichever runs last leaves the same constraints.
alter table notes drop constraint if exists notes_check1;
alter table notes drop constraint if exists notes_to_check;
alter table notes add constraint notes_to_check check (kind = 'question' or system or to_id is null);
alter table notes drop constraint if exists notes_kind_check;
alter table notes add constraint notes_kind_check
  check (kind in ('plan', 'decision', 'interface', 'question', 'answer', 'blocker', 'handoff', 'overlap', 'review', 'ci', 'approval', 'rebase'));
create index if not exists notes_system_to on notes (to_id, created_at) where system and to_id is not null;

-- The tree of each shipped commit (as pushed, on top of the target), so a
-- review comment on a line maps to the change that wrote the line. Null for
-- shipments made before v0.4.
alter table shipment_changes add column if not exists tree bytea;
alter table shipments add column if not exists base_tree bytea;

-- CI results reported for a shipment's commits: one row per check run or
-- commit status context, the latest report wins.
create table shipment_checks (
  shipment_id uuid not null references shipments(id) on delete cascade,
  head_sha text not null,
  source text not null,              -- check_run, status, pipeline
  name text not null,
  status text not null,              -- queued, in_progress, completed
  conclusion text not null default '', -- success, failure, ... once completed
  url text not null default '',
  updated_at timestamptz not null default now(),
  primary key (shipment_id, head_sha, source, name)
);

-- The CI result of a shipment's head commit, summed up from its checks.
create table shipment_ci (
  shipment_id uuid primary key references shipments(id) on delete cascade,
  head_sha text not null,
  state text not null check (state in ('pending', 'passed', 'failed')),
  updated_at timestamptz not null default now()
);

-- The latest system note vx posted about one subject (the CI result of a
-- shipment for one author, an effort's rebase, one review comment), so the
-- next one supersedes it instead of piling up, and an unchanged result
-- (a rerun) posts nothing.
create table system_note_keys (
  effort_id uuid not null references efforts(id) on delete cascade,
  key text not null,
  note_id uuid not null references notes(id) on delete cascade,
  digest text not null default '',
  updated_at timestamptz not null default now(),
  primary key (effort_id, key)
);

-- Permissions or events the server's GitHub App lacks, found when GitHub
-- refused a call (Admin > GitHub shows them). Cleared when a call succeeds
-- or the App is created again.
create table github_app_gaps (
  what text primary key,             -- e.g. 'checks:write'
  detail text not null default '',
  seen_at timestamptz not null default now()
);
