create table if not exists tasks (
  id               text primary key,
  status           text        not null,
  process_type     text        not null,
  finished         boolean     not null default false,
  lease_expires_at timestamptz,
  -- protojson-encoded floatilla.v1.Task (proto field names)
  data             jsonb       not null,
  created_at       timestamptz not null,
  updated_at       timestamptz not null
);

create index if not exists tasks_active on tasks (created_at) where not finished;
create index if not exists tasks_lease on tasks (lease_expires_at) where lease_expires_at is not null;

create table if not exists outbox (
  id         bigserial   primary key,
  subject    text        not null,
  payload    bytea       not null,
  msg_id     text        not null default '',
  core       boolean     not null default false,
  created_at timestamptz not null default now()
);
