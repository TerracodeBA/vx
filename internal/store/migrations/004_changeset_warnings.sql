-- Warnings the server found in a changeset's tree when it landed, such as a
-- Go function declared twice in one package. A JSON array of objects with
-- path, kind, detail (and for duplicates name, decl, files). Kept beside the
-- changeset: not part of its raw encoding or hash. Null means none.
alter table changesets add column warnings jsonb;
