ALTER TABLE project ADD COLUMN source_type TEXT NOT NULL DEFAULT '';
ALTER TABLE project ADD COLUMN git_repository TEXT NOT NULL DEFAULT '';
ALTER TABLE project ADD COLUMN git_branch TEXT NOT NULL DEFAULT '';
ALTER TABLE project ADD COLUMN build_pack TEXT NOT NULL DEFAULT '';
ALTER TABLE project ADD COLUMN dockerfile_blob TEXT NOT NULL DEFAULT '';
ALTER TABLE project ADD COLUMN compose_blob TEXT NOT NULL DEFAULT '';
ALTER TABLE project ADD COLUMN ports_exposes TEXT NOT NULL DEFAULT '';
ALTER TABLE project ADD COLUMN ttl_seconds INTEGER;
ALTER TABLE project ADD COLUMN reset_interval_seconds INTEGER;
ALTER TABLE project ADD COLUMN expiry_action TEXT NOT NULL DEFAULT 'stop';

ALTER TABLE instance ADD COLUMN expires_at INTEGER;
ALTER TABLE instance ADD COLUMN next_reset_at INTEGER;

CREATE TABLE instance_event (
  id          INTEGER PRIMARY KEY,
  instance_id INTEGER REFERENCES instance(id) ON DELETE CASCADE,
  project_id  INTEGER NOT NULL,
  kind        TEXT NOT NULL,
  detail      TEXT,
  created_at  INTEGER NOT NULL
);
