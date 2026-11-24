-- v0.3 contract: efforts see each other (#26) and board polish (#27).
-- Agents add 017 (context), 018 (overlaps), 019 (rules), 020 (polish) only if they need more.

-- Notes vx posts itself (overlaps now; review comments and CI results in v0.4).
alter table notes drop constraint if exists notes_kind_check;
alter table notes add constraint notes_kind_check
  check (kind in ('plan', 'decision', 'interface', 'question', 'answer', 'blocker', 'handoff', 'overlap', 'review', 'ci'));
alter table notes add column supersedes uuid references notes(id) on delete set null;
alter table notes add column system boolean not null default false; -- posted by vx, not by a person or agent
create index notes_supersedes on notes (supersedes) where supersedes is not null;

-- What each principal has seen: notes they were shown or answered.
create table note_reads (
  principal_id uuid not null references principals(id) on delete cascade,
  note_id uuid not null references notes(id) on delete cascade,
  read_at timestamptz not null default now(),
  primary key (principal_id, note_id)
);

-- Forecast collisions between two open efforts of one repository.
create table effort_overlaps (
  repo_id uuid not null references repos(id) on delete cascade,
  effort_a uuid not null references efforts(id) on delete cascade,
  effort_b uuid not null references efforts(id) on delete cascade,
  status text not null check (status in ('clean', 'overlap', 'conflict')),
  shared_files text[] not null default '{}', -- changed by both since their bases
  conflicts text[] not null default '{}', -- files that would not merge
  warnings jsonb not null default '[]', -- rule violations and semantic warnings of the combination
  a_head text not null, -- changeset hashes the forecast was computed for
  b_head text not null,
  checked_at timestamptz not null default now(),
  primary key (effort_a, effort_b),
  check (effort_a < effort_b)
);

-- Values handed out by vx.toml [[unique]] rules (for example migration numbers).
create table rule_reservations (
  repo_id uuid not null references repos(id) on delete cascade,
  rule text not null,
  value text not null,
  effort_id uuid references efforts(id) on delete set null,
  reserved_by uuid references principals(id) on delete set null,
  created_at timestamptz not null default now(),
  primary key (repo_id, rule, value)
);

-- The principal that posts system notes.
insert into principals (kind, handle, display_name) values ('service', 'vx', 'vx') on conflict (handle) do nothing;
