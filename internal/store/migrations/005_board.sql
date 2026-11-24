-- The board: typed notes agents leave for each other in an effort. A thread
-- is a root note and its answers; a root's thread_id is its own id. The
-- state "escalated" is never stored: an open question past answer_by reads
-- as escalated.
create table notes (
    id         uuid primary key,
    seq        bigint generated always as identity, -- tie-breaker for ordering and paging
    repo_id    uuid not null references repos on delete cascade,
    effort_id  uuid not null references efforts on delete cascade,
    thread_id  uuid not null references notes on delete cascade,
    kind       text not null check (kind in ('plan', 'decision', 'interface', 'question', 'answer', 'blocker', 'handoff')),
    targets    text[] not null default '{}',
    author_id  uuid not null references principals,
    to_id      uuid references principals,
    body       text not null check (length(body) between 1 and 2000),
    state      text not null default 'open' check (state in ('open', 'answered', 'closed')),
    answer_by  timestamptz,
    created_at timestamptz not null default now(),
    check ((kind = 'question') = (answer_by is not null)),
    check (kind = 'question' or to_id is null)
);
create index notes_effort_time on notes (effort_id, created_at desc, seq desc);
create index notes_targets on notes using gin (targets);
create index notes_thread on notes (thread_id);
create index notes_inbox on notes (to_id, state) where to_id is not null;
create index notes_author_questions on notes (author_id) where kind = 'question';
create index notes_open_questions on notes (answer_by) where kind = 'question' and state = 'open';
