-- deleted_accounts: the id of each account whose deletion has begun, when,
-- and the deletion's own value (random, made for it, a new one for each
-- deletion), so that nothing more is kept for it. A write of a person's row
-- (deleted.go, asAccount) is refused while the account's row is younger than
-- an hour (DeletedAccountKept); an older row is ignored, and removed by the
-- next write or deletion. The deletion writes its row in its own
-- transaction: a deletion rolled back leaves none, and one whose account
-- stays after all is forgotten by its value (ForgetDeletion), so that an id
-- another deletion has kept since stays. Nothing else of the account is
-- kept here. user_id is text, as wide as a subject can be (OpenID Connect
-- allows 255 characters), so that the id's insert never fails where the
-- deletion's deletes ran.
BEGIN;

CREATE TABLE IF NOT EXISTS deleted_accounts (
  user_id    TEXT        PRIMARY KEY,
  deleted_at timestamptz NOT NULL,
  deletion   VARCHAR(64) NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_deleted_accounts_deleted_at
  ON deleted_accounts (deleted_at);

COMMIT;
