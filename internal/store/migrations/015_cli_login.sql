-- Browser login for the CLI: rate limiting and polling speed.
-- The CLI's token is created when the CLI collects it after approval, inside
-- the same transaction that marks the login used, so token_secret_enc from
-- 011 stays empty: no token, encrypted or not, ever rests in this table.

alter table cli_logins
  add column client_ip text not null default '', -- who started it, for the per-address rate limit
  add column last_polled_at timestamptz, -- for "slow down" when a client polls too fast
  add column decided_at timestamptz;

create index cli_logins_ip_created on cli_logins (client_ip, created_at);
create index cli_logins_expires on cli_logins (expires_at);
