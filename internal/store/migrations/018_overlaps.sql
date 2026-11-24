-- Overlap forecasts (#26): the target head a forecast was computed on, and
-- the overlap note it posted on each effort's board, so the next note for
-- the pair supersedes it instead of piling up.
alter table effort_overlaps add column target_head text not null default '';
alter table effort_overlaps add column a_note uuid references notes(id) on delete set null;
alter table effort_overlaps add column b_note uuid references notes(id) on delete set null;
create index effort_overlaps_b on effort_overlaps (effort_b);
create index effort_overlaps_repo on effort_overlaps (repo_id);
