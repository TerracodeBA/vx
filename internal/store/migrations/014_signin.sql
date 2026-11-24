-- Sign in with GitHub: the first-run welcome page stays linked from the
-- navigation until its person dismisses it.

alter table principals
  add column welcome_dismissed_at timestamptz;

-- Invites made on the server console can make the person a site admin, so
-- a new server's first admin can sign up with GitHub.
alter table signup_invites
  add column site_admin boolean not null default false;
