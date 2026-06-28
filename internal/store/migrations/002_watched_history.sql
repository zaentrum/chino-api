-- watched_history: one row per (user, item) marked as fully watched.
-- Distinct from playback_progress, which carries position. A finished
-- movie/episode appears in both: progress row at near-end position
-- (filtered out of Continue Watching) and a watched_history row
-- (surfaces a "Watched" indicator on MediaCard).
BEGIN;

CREATE TABLE IF NOT EXISTS watched_history (
  user_id    VARCHAR(64) NOT NULL,
  item_id    VARCHAR(36) NOT NULL,
  watched_at TIMESTAMP   NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, item_id)
);

CREATE INDEX IF NOT EXISTS idx_watched_history_user_watched_at
  ON watched_history (user_id, watched_at DESC);

COMMIT;
