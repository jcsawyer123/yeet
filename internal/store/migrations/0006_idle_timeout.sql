ALTER TABLE project ADD COLUMN idle_timeout_seconds INTEGER;
ALTER TABLE instance ADD COLUMN idle_expires_at INTEGER;
