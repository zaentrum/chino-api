-- The settings an account keeps on the server, the same on every device it
-- signs in on: one row per person, by their subject, as every other table
-- here keys a person, and a column per setting. A person without a row has
-- every setting at its first value. Deleted with the rest of the person's
-- data (DELETE /api/v1/me).
--
-- watchlist_by_kind: whether every list shows its movies and its shows
-- apart (Separate movies and shows), off at first.
BEGIN;

CREATE TABLE IF NOT EXISTS account_settings (
  user_id           VARCHAR(64) PRIMARY KEY,
  watchlist_by_kind BOOLEAN     NOT NULL DEFAULT false,
  updated_at        TIMESTAMP   NOT NULL DEFAULT now()
);

COMMIT;
