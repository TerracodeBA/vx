-- Webhook deliveries already applied, by X-GitHub-Delivery, so a replayed
-- or redelivered webhook is applied once. Rows older than a few days are
-- pruned (deliveries older than that are refused as stale anyway).
create table github_deliveries (
  id text primary key,
  event text not null,
  received_at timestamptz not null default now()
);

create index github_deliveries_received on github_deliveries (received_at);
