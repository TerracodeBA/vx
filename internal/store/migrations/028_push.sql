-- Web Push notifications (v0.5, #37): the server's VAPID key, the
-- browsers people turned notifications on in, and what was pushed.

-- One row: the server's VAPID key. The private scalar is sealed with the
-- server key (secret purpose "push.vapid_key").
create table push_keys (
    id              smallint primary key default 1 check (id = 1),
    private_key_enc bytea not null,
    public_key      text not null, -- base64url uncompressed P-256 point
    created_at      timestamptz not null default now()
);

-- A browser on a device that receives a person's notifications.
create table push_subscriptions (
    id           uuid primary key default gen_random_uuid(),
    principal_id uuid not null references principals on delete cascade,
    endpoint     text not null unique,
    p256dh       text not null,
    auth         text not null,
    device       text not null default '', -- what the browser said it is, e.g. "Chrome on Android"
    created_at   timestamptz not null default now(),
    last_sent_at timestamptz,
    last_error   text not null default ''
);
create index push_subscriptions_principal on push_subscriptions (principal_id);

-- What a person was notified of ("question:<id>", "review:<id>"), so each
-- thing is pushed once, by one server instance.
create table push_sent (
    principal_id uuid not null references principals on delete cascade,
    item         text not null,
    sent_at      timestamptz not null default now(),
    primary key (principal_id, item)
);
