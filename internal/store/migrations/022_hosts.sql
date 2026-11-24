-- GitLab, Gitea, Forgejo and Bitbucket Cloud upstreams (#8).
alter table upstreams drop constraint upstreams_provider_check;
alter table upstreams add constraint upstreams_provider_check
    check (provider in ('github', 'git', 'none', 'gitlab', 'gitea', 'forgejo', 'bitbucket'));

-- The host's web address, e.g. https://gitlab.example.com; empty for
-- GitHub, plain git and vx-only repositories.
alter table upstreams add column base_url text not null default '';
-- A token for this repository only (sealed with VX_SECRET_KEY, purpose
-- "upstream.token:<repo id>"); null: the server's token for the host.
alter table upstreams add column token_enc bytea;
-- The secret the host signs webhook deliveries with (sealed, purpose
-- "upstream.webhook_secret:<repo id>"); null: the server's secret.
alter table upstreams add column webhook_secret_enc bytea;

create index upstreams_provider_remote on upstreams (provider, lower(remote));
