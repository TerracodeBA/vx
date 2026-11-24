-- Coordination that never fails silently.

-- When a workspace last did something (sync, land, status). Null: since
-- it was created. Claims tied to an open workspace lapse after it has been
-- idle for two hours.
alter table workspaces add column active_at timestamptz;

-- A question to an agent that was not working when it was asked is copied
-- to that agent's person at once; cc_id is that person.
alter table notes add column cc_id uuid references principals on delete set null;
create index notes_cc_open on notes (cc_id) where cc_id is not null and state = 'open';

-- vx claim --moving pkg.Func --to path: where the claimed code is going.
alter table claims add column moving_to text not null default '';

-- 'claim': vx tells the owner that a claim lapsed.
alter table notes drop constraint if exists notes_kind_check;
alter table notes add constraint notes_kind_check
  check (kind in ('plan', 'decision', 'interface', 'question', 'answer', 'blocker', 'handoff', 'overlap', 'review', 'ci', 'approval', 'rebase', 'claim'));
