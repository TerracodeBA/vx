-- Deleting an organization (#40). An owner deletes it: it is hidden at
-- once (its repositories are "not found" everywhere, it leaves every list,
-- shipping and syncing stop) and its handle stays reserved. An owner can
-- restore it until purge_after. After that a background job purges it:
-- every row of its repositories and of the organization itself, git
-- mirrors and the blobs no other repository uses. What is left is the
-- deletion record below.

alter table orgs
  add column deleted_at  timestamptz,
  add column deleted_by  uuid references principals(id) on delete set null,
  add column purge_after timestamptz;
create index orgs_deleted on orgs (purge_after) where deleted_at is not null;

-- The record of a deletion, kept after the purge so the deletion can be
-- confirmed in writing. It holds no repository names or content: the
-- organization's id and handle, who asked, when, and how many of each kind
-- of record the purge removed. No foreign key to orgs: the row outlives it.
-- A restore removes the row (the operation log keeps org.delete and
-- org.restore).
create table org_deletions (
  org_id         uuid primary key,
  handle         text not null,
  requested_by   text not null,               -- principal ref ("user:alice"), kept if the person goes
  deleted_at     timestamptz not null,
  purge_after    timestamptz not null,
  purge_started  timestamptz,
  purged_at      timestamptz,
  purged_by      text not null default '',     -- "schedule" or "console" (vx-server org purge --now)
  step           text not null default '',     -- where a running purge is, to resume after a crash
  counts         jsonb not null default '{}'   -- what the purge removed, by kind
);

-- Blobs a purge found no longer used by any repository. The purger deletes
-- each one only after checking again, under an advisory lock on the hash,
-- that no repository records it; code that records objects for a
-- repository checks this table after recording and puts back anything a
-- purge deleted meanwhile (store.RecordRepoObjects). Rows stay a day after
-- the blob was deleted so a recording that started before sees them.
create table blob_deletions (
  hash       bytea primary key,
  marked_at  timestamptz not null default now(),
  deleted_at timestamptz
);
create index blob_deletions_pending on blob_deletions (marked_at) where deleted_at is null;
