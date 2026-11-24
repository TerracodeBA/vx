-- Split shipments (#16): an effort shipped as several pull requests has one
-- shipment per part. split says how the parts relate ('independent': they
-- merge in any order; 'stacked': part n is based on part n-1's branch) and
-- part is the 1-based position; an effort shipped as one pull request has
-- split '' and part 0.
alter table shipments add column split text not null default ''
  check (split in ('', 'independent', 'stacked'));
alter table shipments add column part int not null default 0 check (part >= 0);
