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

-- Story tracker sync (internal/tracker): one row per tracker item floatilla
-- knows about, and the plans whose stories were published.
create table if not exists tracker_links (
  item_id      text        primary key,
  item_key     text        not null default '',
  url          text        not null default '',
  task_id      text        not null default '',
  plan_task_id text        not null default '',
  story_key    text        not null default '',
  synced_state text        not null default '',
  since        timestamptz not null default now(),
  accepted     boolean     not null default false,
  pr_linked    boolean     not null default false,
  dispatches   integer     not null default 0,
  settled      boolean     not null default false
);

create table if not exists tracker_plans (
  plan_task_id text        primary key,
  published_at timestamptz not null default now()
);
