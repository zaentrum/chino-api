-- feedback_reports: dedup ledger for the bug-report pipeline
-- (POST /api/v1/feedback). One row per error fingerprint, pointing at
-- the OpenProject work package tracking it. A repeat report within the
-- dedup window bumps count/last_seen and comments on the existing
-- ticket instead of opening a new one; older fingerprints (or deleted
-- tickets) get a fresh work package and the row is re-pointed.
BEGIN;

CREATE TABLE IF NOT EXISTS feedback_reports (
  fingerprint     text        PRIMARY KEY,
  work_package_id bigint      NOT NULL,
  count           integer     NOT NULL DEFAULT 1,
  first_seen      timestamptz NOT NULL DEFAULT now(),
  last_seen       timestamptz NOT NULL DEFAULT now()
);

COMMIT;
