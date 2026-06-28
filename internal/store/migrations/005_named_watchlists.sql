-- Multiple named watchlists. The old single `watchlist` table (per-user
-- flag rows) becomes a per-user "default list"; this migration models
-- lists as first-class rows and moves item membership into a child table.
--
-- The legacy `watchlist` table is left in place untouched for rollback
-- safety — the watchlist path stops reading it once 005 lands, but the
-- data migration below copies every existing row into the new shape so
-- nothing is lost. /me/likes stays on the old `likes` flag table.
BEGIN;

CREATE TABLE IF NOT EXISTS watchlists (
  id         TEXT        PRIMARY KEY,
  user_id    VARCHAR(64) NOT NULL,
  name       TEXT        NOT NULL,
  is_default BOOLEAN     NOT NULL DEFAULT false,
  created_at TIMESTAMP   NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_watchlists_user_created_at
  ON watchlists (user_id, created_at);

-- At most one default list per user. Partial unique index so the app
-- can never wind up with two "Watchlist" defaults for the same subject.
CREATE UNIQUE INDEX IF NOT EXISTS uq_watchlists_one_default_per_user
  ON watchlists (user_id) WHERE is_default;

CREATE TABLE IF NOT EXISTS watchlist_items (
  list_id  TEXT        NOT NULL REFERENCES watchlists(id) ON DELETE CASCADE,
  item_id  VARCHAR(36) NOT NULL,
  added_at TIMESTAMP   NOT NULL DEFAULT now(),
  PRIMARY KEY (list_id, item_id)
);

CREATE INDEX IF NOT EXISTS idx_watchlist_items_list_added_at
  ON watchlist_items (list_id, added_at DESC);

-- Data migration: give every user who already has legacy watchlist rows
-- a default list, then copy their items into it. Deterministic list id
-- ('dflt_' || left(md5(user_id),24)) so a re-run targets the same row and
-- ON CONFLICT DO NOTHING keeps the whole step idempotent.
INSERT INTO watchlists (id, user_id, name, is_default, created_at)
SELECT 'dflt_' || left(md5(w.user_id), 24),
       w.user_id,
       'Watchlist',
       true,
       now()
FROM (SELECT DISTINCT user_id FROM watchlist) w
ON CONFLICT (id) DO NOTHING;

INSERT INTO watchlist_items (list_id, item_id, added_at)
SELECT 'dflt_' || left(md5(w.user_id), 24),
       w.item_id,
       w.added_at
FROM watchlist w
ON CONFLICT (list_id, item_id) DO NOTHING;

COMMIT;
