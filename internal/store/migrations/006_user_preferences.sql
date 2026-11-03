-- What a person has set for their account, the same on every device. A
-- person without a row has every setting at its default.
BEGIN;

CREATE TABLE IF NOT EXISTS user_preferences (
  user_id           VARCHAR(64) PRIMARY KEY,
  watchlist_by_kind BOOLEAN     NOT NULL DEFAULT false,
  updated_at        TIMESTAMP   NOT NULL DEFAULT now()
);

COMMIT;
