-- A question asked while a similar one is open in the effort (same --on
-- targets, nearly the same words) points at it, so the board links the two
-- and the asker is told (#48).
alter table notes add column similar_to uuid references notes(id) on delete set null;
create index notes_similar_to on notes (similar_to) where similar_to is not null;
