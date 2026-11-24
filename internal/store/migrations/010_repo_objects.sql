-- Objects (blobs and trees) stay in one deduplicated, content-addressed
-- store. Which repository an object belongs to is recorded here: an object
-- is served only through a repository it is a member of, to callers who can
-- read that repository.
create table repo_objects (
    repo_id uuid not null references repos on delete cascade,
    hash    bytea not null,
    primary key (repo_id, hash)
);

-- Repositories whose objects predate repo_objects. The server walks their
-- changesets' trees once, records the objects, and deletes the row. Until
-- then, object requests for the repository wait for that walk.
create table repo_objects_backfill (
    repo_id   uuid primary key references repos on delete cascade,
    queued_at timestamptz not null default now()
);
insert into repo_objects_backfill (repo_id) select id from repos;
