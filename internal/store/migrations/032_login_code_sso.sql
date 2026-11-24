-- v0.5: a vx ui sign-in code remembers the token that asked for it, so the
-- browser session it starts inherits that token's single sign-on.
alter table login_codes add column token_id uuid references tokens(id) on delete cascade;
