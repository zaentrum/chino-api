-- chino-api's own database. One table per concept; no service prefix on
-- table names because the database itself is the chino scope.
BEGIN;

CREATE TABLE IF NOT EXISTS playback_progress (
  user_id      VARCHAR(64)  NOT NULL,
  item_id      VARCHAR(36)  NOT NULL,
  position_sec INTEGER      NOT NULL,
  duration_sec INTEGER      NOT NULL DEFAULT 0,
  updated_at   TIMESTAMP    NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, item_id)
);

CREATE INDEX IF NOT EXISTS idx_playback_progress_user_updated
  ON playback_progress (user_id, updated_at DESC);

COMMIT;
