-- watchlist: items the user wants to watch later.
-- likes: items the user has thumbed up.
-- Two separate tables so a UI distinguishing them stays trivial; both
-- share the same shape (user, item, added_at).
BEGIN;

CREATE TABLE IF NOT EXISTS watchlist (
  user_id  VARCHAR(64) NOT NULL,
  item_id  VARCHAR(36) NOT NULL,
  added_at TIMESTAMP   NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, item_id)
);

CREATE INDEX IF NOT EXISTS idx_watchlist_user_added_at
  ON watchlist (user_id, added_at DESC);

CREATE TABLE IF NOT EXISTS likes (
  user_id  VARCHAR(64) NOT NULL,
  item_id  VARCHAR(36) NOT NULL,
  liked_at TIMESTAMP   NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, item_id)
);

CREATE INDEX IF NOT EXISTS idx_likes_user_liked_at
  ON likes (user_id, liked_at DESC);

COMMIT;
