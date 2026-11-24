-- Bytes each object takes in the blob store (compressed, as stored on disk
-- or in the bucket), for storage quotas. Objects are shared across
-- repositories, so sizes are per object; repo_objects says who uses them.
-- The server fills in objects recorded without a size in the background.
create table object_sizes (
    hash  bytea primary key,
    bytes bigint not null check (bytes >= 0)
);

-- One-time web sign-in codes (vx ui), kept in the database so that any of
-- several servers behind a load balancer can redeem a code another one
-- issued. Only a sha256 of each code is stored.
create table login_codes (
    code_hash    bytea primary key,
    principal_id uuid not null references principals on delete cascade,
    expires_at   timestamptz not null
);
create index login_codes_expires on login_codes (expires_at);
