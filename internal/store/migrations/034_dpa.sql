-- v0.5: the data processing agreement (#41). An organization's owner
-- accepts a version of the agreement the operator publishes (VX_DPA_DIR);
-- each acceptance is kept for as long as the organization exists. The
-- accepted texts are kept once each, by their SHA-256, so the text an
-- owner accepted can be shown and downloaded exactly as it was even after
-- the operator's files change. They hold no customer data.
create table dpa_texts (
    sha256     text primary key,
    body       bytea not null,
    created_at timestamptz not null default now()
);

create table org_dpa_acceptances (
    id              uuid primary key default gen_random_uuid(),
    org_id          uuid not null references orgs(id) on delete cascade,
    version_id      text not null,
    sha256          text not null references dpa_texts(sha256),
    accepted_by     uuid references principals(id) on delete set null,
    accepted_by_ref text not null,
    accepted_at     timestamptz not null default now(),
    ip              text not null default '',
    unique (org_id, version_id)
);
