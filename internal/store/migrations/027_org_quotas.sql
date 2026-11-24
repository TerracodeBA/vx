-- Quotas per organization (#38). The server's defaults come from its
-- configuration (VX_QUOTA_*); a row here overrides them for one
-- organization. A null column keeps the default; 0 means no limit.
create table org_quotas (
    org_id       uuid primary key references orgs on delete cascade,
    repos        bigint check (repos >= 0),        -- repositories
    stored_bytes bigint check (stored_bytes >= 0), -- bytes in the object store, all repositories
    import_bytes bigint check (import_bytes >= 0), -- one import from a git host
    push_bytes   bigint check (push_bytes >= 0),   -- one upload of objects
    open_efforts bigint check (open_efforts >= 0), -- efforts not merged or abandoned
    updated_by   uuid references principals on delete set null,
    updated_at   timestamptz not null default now()
);

-- Audit export filters by time and actor, and retention deletes by time.
create index op_log_at on op_log (at);
create index op_log_actor on op_log (actor_id, id);
