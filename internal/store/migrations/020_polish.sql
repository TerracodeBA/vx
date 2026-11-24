-- v0.3 board polish (#27).

-- Interface notes carry signatures and may be up to 4000 characters; other
-- notes stay at 2000.
alter table notes drop constraint if exists notes_body_check;
alter table notes add constraint notes_body_check
  check (length(body) >= 1 and length(body) <= case when kind = 'interface' then 4000 else 2000 end);

-- The paths an open workspace has changed locally, as its working copy last
-- reported them (vx status, vx sync, vx claim); null: never reported.
alter table workspaces add column changed_paths text[];
alter table workspaces add column changed_paths_at timestamptz;
