-- Mapping between git objects and vx objects, per repo. Lets repeated
-- imports skip unchanged subtrees and lets exports reuse git objects.
create table git_objects (
    repo_id  uuid not null references repos on delete cascade,
    git_hash text not null,          -- 40 hex characters
    vx_hash  bytea not null,
    kind     text not null check (kind in ('blob', 'tree')),
    primary key (repo_id, git_hash)
);
create index git_objects_vx on git_objects (repo_id, vx_hash);
