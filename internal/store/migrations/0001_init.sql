CREATE TABLE project (
  id         INTEGER PRIMARY KEY,
  slug       TEXT NOT NULL UNIQUE,
  name       TEXT NOT NULL,
  kind       TEXT NOT NULL DEFAULT 'adhoc',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE instance (
  id             INTEGER PRIMARY KEY,
  project_id     INTEGER NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  short_id       TEXT NOT NULL,
  coolify_uuid   TEXT NOT NULL UNIQUE,
  coolify_kind   TEXT NOT NULL,
  fqdn           TEXT,
  observed_state TEXT,
  observed_at    INTEGER,
  created_at     INTEGER NOT NULL,
  deleted_at     INTEGER
);

CREATE INDEX idx_instance_project_live ON instance(project_id, deleted_at);
