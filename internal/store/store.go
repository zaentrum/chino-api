// Package store is chino-api's tiny Postgres layer. Owns user-state
// tables (playback progress today, watchlist + history later). Lives in
// its own `chino` database on the acid Postgres cluster — one DB per
// microservice, accessed as cloud_chinoapi.
package store

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ p *pgxpool.Pool }

// New connects to Postgres and pings once to fail fast on bad creds.
// Returns nil + nil error when pgURL is empty so the rest of chino-api
// can decide whether to disable progress/telemetry features gracefully.
func New(ctx context.Context, pgURL string) (*Store, error) {
	if pgURL == "" {
		log.Println("store: PG_URL empty — running without persistence")
		return nil, nil
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := pgxpool.New(pingCtx, pgURL)
	if err != nil {
		return nil, err
	}
	if err := p.Ping(pingCtx); err != nil {
		return nil, err
	}
	return &Store{p: p}, nil
}

func (s *Store) Close() {
	if s != nil && s.p != nil {
		s.p.Close()
	}
}

// GetProgress returns the last saved playback position (seconds) for
// (user, item). 0 means "never watched". A nil Store always returns 0 —
// callers don't need to special-case the no-DB development path.
func (s *Store) GetProgress(ctx context.Context, userID, itemID string) (int, error) {
	if s == nil || s.p == nil || userID == "" || itemID == "" {
		return 0, nil
	}
	var pos int
	err := s.p.QueryRow(ctx,
		`SELECT position_sec FROM playback_progress
		 WHERE user_id = $1 AND item_id = $2`, userID, itemID).Scan(&pos)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return pos, err
}

// ProgressRow is one row from the playback_progress table.
// Finished is true when the saved position is within 60 s of the saved
// duration (and duration is known). The handler uses this flag to
// decide whether to surface the row as-is (in-progress) or substitute
// the parent series' next episode (finished episode) / drop it
// (finished movie).
type ProgressRow struct {
	ItemID      string
	PositionSec int
	DurationSec int
	Finished    bool
}

// ListContinueWatching returns the rows for items the user has progress
// on, most-recently-watched first. Includes both in-progress AND
// finished entries (with Finished flagged) so the handler can decide
// whether to substitute the next episode for a finished one.
func (s *Store) ListContinueWatching(ctx context.Context, userID string, limit int) ([]ProgressRow, error) {
	if s == nil || s.p == nil || userID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 40
	}
	// "Finished" means the row should drive Next-Up substitution rather
	// than appearing as a half-watched card. Two routes produce that
	// state:
	//   1. The player auto-marks at ≥95 % of duration → position lands
	//      within 60 s of the saved duration.
	//   2. The user clicks the watched toggle (chino-web DetailPage /
	//      EpisodesList) at an arbitrary position → a watched_history
	//      row exists, but position may be well short of the end (e.g.
	//      they stopped during credits).
	// Either path must trigger the same handler-side branch (drop
	// movies, substitute next episode for series). So we treat
	// "in watched_history" as equivalent to "at-end" for the finished
	// flag, and we no longer drop watched rows from the result set —
	// the handler decides what to do with them.
	rows, err := s.p.Query(ctx,
		`SELECT p.item_id, p.position_sec, p.duration_sec,
		        (
		          (p.duration_sec > 0 AND p.position_sec >= p.duration_sec - 60)
		          OR EXISTS (
		            SELECT 1 FROM watched_history w
		            WHERE w.user_id = p.user_id AND w.item_id = p.item_id
		          )
		        ) AS finished
		 FROM playback_progress p
		 WHERE p.user_id = $1
		   AND p.position_sec > 30
		 ORDER BY p.updated_at DESC
		 LIMIT $2`,
		userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProgressRow
	for rows.Next() {
		var p ProgressRow
		if err := rows.Scan(&p.ItemID, &p.PositionSec, &p.DurationSec, &p.Finished); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkWatched inserts (or refreshes) a watched_history row. Idempotent:
// re-watching bumps watched_at to now() so the most-recent watch wins.
func (s *Store) MarkWatched(ctx context.Context, userID, itemID string) error {
	if s == nil || s.p == nil || userID == "" || itemID == "" {
		return nil
	}
	_, err := s.p.Exec(ctx,
		`INSERT INTO watched_history (user_id, item_id, watched_at)
		 VALUES ($1, $2, now())
		 ON CONFLICT (user_id, item_id) DO UPDATE
		   SET watched_at = now()`,
		userID, itemID)
	return err
}

// WatchedRow is one entry of the user's watch history — what they
// watched and when. The handler joins these against katalog metadata
// before returning to the client.
type WatchedRow struct {
	ItemID    string
	WatchedAt time.Time
}

// ListWatched returns the user's watch history, most-recently-watched
// first. Used by the chino-web ProfilePage to render a "things you've
// watched" grid. Pagination via limit/offset so a heavy watcher's
// history doesn't pull the whole table in one shot.
func (s *Store) ListWatched(ctx context.Context, userID string, limit, offset int) ([]WatchedRow, error) {
	if s == nil || s.p == nil || userID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.p.Query(ctx,
		`SELECT item_id, watched_at
		 FROM watched_history
		 WHERE user_id = $1
		 ORDER BY watched_at DESC
		 LIMIT $2 OFFSET $3`,
		userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WatchedRow
	for rows.Next() {
		var r WatchedRow
		if err := rows.Scan(&r.ItemID, &r.WatchedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UnmarkWatched removes a watched_history row, so the item stops
// showing the "watched" badge and re-enters Continue Watching if a
// progress row still exists. Idempotent — deleting an absent row is a
// no-op.
func (s *Store) UnmarkWatched(ctx context.Context, userID, itemID string) error {
	if s == nil || s.p == nil || userID == "" || itemID == "" {
		return nil
	}
	_, err := s.p.Exec(ctx,
		`DELETE FROM watched_history WHERE user_id = $1 AND item_id = $2`,
		userID, itemID)
	return err
}

// WatchedAtBatch returns the watched_at timestamp for each requested
// item id the user has marked. Items the user has not watched are
// absent from the result map. A nil store / empty input returns an
// empty map so callers can iterate without nil checks.
func (s *Store) WatchedAtBatch(ctx context.Context, userID string, itemIDs []string) (map[string]time.Time, error) {
	out := make(map[string]time.Time)
	if s == nil || s.p == nil || userID == "" || len(itemIDs) == 0 {
		return out, nil
	}
	rows, err := s.p.Query(ctx,
		`SELECT item_id, watched_at FROM watched_history
		 WHERE user_id = $1 AND item_id = ANY($2)`,
		userID, itemIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var ts time.Time
		if err := rows.Scan(&id, &ts); err != nil {
			return nil, err
		}
		out[id] = ts
	}
	return out, rows.Err()
}

// LastWatchedEpisode picks the most recently updated episode id within a
// candidate set (typically every episode of a series). Used by
// /series/{id}/next-episode to find where the user left off when no
// explicit "after" anchor is provided.
func (s *Store) LastWatchedEpisode(ctx context.Context, userID string, candidates []string) (string, error) {
	if s == nil || s.p == nil || userID == "" || len(candidates) == 0 {
		return "", nil
	}
	var id string
	err := s.p.QueryRow(ctx,
		`SELECT item_id FROM playback_progress
		 WHERE user_id = $1 AND item_id = ANY($2)
		 ORDER BY updated_at DESC
		 LIMIT 1`, userID, candidates).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// SetFlag toggles a user→item relationship row in one of the simple
// "tag tables" (watchlist, likes). Pass present=true to upsert, false
// to delete. Idempotent either way. `table` MUST be a literal from the
// caller (we don't accept arbitrary identifiers; see WatchlistTable /
// LikesTable constants).
func (s *Store) SetFlag(ctx context.Context, table, userID, itemID string, present bool) error {
	if s == nil || s.p == nil || userID == "" || itemID == "" {
		return nil
	}
	if table != WatchlistTable && table != LikesTable {
		return errors.New("invalid flag table")
	}
	if !present {
		_, err := s.p.Exec(ctx,
			"DELETE FROM "+table+" WHERE user_id = $1 AND item_id = $2",
			userID, itemID)
		return err
	}
	ts := "added_at"
	if table == LikesTable {
		ts = "liked_at"
	}
	_, err := s.p.Exec(ctx,
		"INSERT INTO "+table+" (user_id, item_id, "+ts+") VALUES ($1, $2, now()) "+
			"ON CONFLICT (user_id, item_id) DO NOTHING",
		userID, itemID)
	return err
}

// ListFlag returns the items in the user's watchlist / likes,
// newest-first. Used by the UI to render the "My list" + "Liked" rails.
func (s *Store) ListFlag(ctx context.Context, table, userID string, limit int) ([]string, error) {
	if s == nil || s.p == nil || userID == "" {
		return nil, nil
	}
	if table != WatchlistTable && table != LikesTable {
		return nil, errors.New("invalid flag table")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	ts := "added_at"
	if table == LikesTable {
		ts = "liked_at"
	}
	rows, err := s.p.Query(ctx,
		"SELECT item_id FROM "+table+" WHERE user_id = $1 ORDER BY "+ts+" DESC LIMIT $2",
		userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FlagsForItems returns the subset of itemIDs that appear in the user's
// table. Used to hydrate MediaCard + DetailPage with the current state
// in one round-trip instead of one /flag query per item.
func (s *Store) FlagsForItems(ctx context.Context, table, userID string, itemIDs []string) (map[string]bool, error) {
	out := make(map[string]bool)
	if s == nil || s.p == nil || userID == "" || len(itemIDs) == 0 {
		return out, nil
	}
	if table != WatchlistTable && table != LikesTable {
		return nil, errors.New("invalid flag table")
	}
	rows, err := s.p.Query(ctx,
		"SELECT item_id FROM "+table+" WHERE user_id = $1 AND item_id = ANY($2)",
		userID, itemIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// Flag table identifiers. Kept as constants (not free strings) so the
// runtime can't accidentally route into a typo table.
const (
	WatchlistTable = "watchlist"
	LikesTable     = "likes"
)

// FeedbackReportRow tracks one deduplicated error fingerprint and the
// OpenProject work package it maps to. The dedup-window logic lives in
// internal/http/feedback.go; the store just keeps the bookkeeping.
type FeedbackReportRow struct {
	Fingerprint   string
	WorkPackageID int
	Count         int
	FirstSeen     time.Time
	LastSeen      time.Time
}

// GetFeedbackReport returns the dedup row for a fingerprint, or nil
// when the fingerprint has never been reported. A nil Store returns
// nil — the handler degrades to "always open a fresh ticket" without
// special-casing the no-DB development path.
func (s *Store) GetFeedbackReport(ctx context.Context, fingerprint string) (*FeedbackReportRow, error) {
	if s == nil || s.p == nil || fingerprint == "" {
		return nil, nil
	}
	var r FeedbackReportRow
	err := s.p.QueryRow(ctx,
		`SELECT fingerprint, work_package_id, count, first_seen, last_seen
		 FROM feedback_reports
		 WHERE fingerprint = $1`, fingerprint).
		Scan(&r.Fingerprint, &r.WorkPackageID, &r.Count, &r.FirstSeen, &r.LastSeen)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// UpsertFeedbackReport points a fingerprint at a (new) work package and
// resets the recurrence counter. Used both for first-time reports and
// for re-pointing after the old ticket was deleted in OpenProject or
// aged out of the dedup window.
func (s *Store) UpsertFeedbackReport(ctx context.Context, fingerprint string, workPackageID int) error {
	if s == nil || s.p == nil || fingerprint == "" {
		return nil
	}
	_, err := s.p.Exec(ctx,
		`INSERT INTO feedback_reports (fingerprint, work_package_id, count, first_seen, last_seen)
		 VALUES ($1, $2, 1, now(), now())
		 ON CONFLICT (fingerprint) DO UPDATE
		   SET work_package_id = EXCLUDED.work_package_id,
		       count           = 1,
		       first_seen      = now(),
		       last_seen       = now()`,
		fingerprint, workPackageID)
	return err
}

// BumpFeedbackReport records a recurrence of a known fingerprint and
// returns the new total count (surfaced in the "Recurred" ticket
// comment). Absent row / nil Store → count 0, no error.
func (s *Store) BumpFeedbackReport(ctx context.Context, fingerprint string) (int, error) {
	if s == nil || s.p == nil || fingerprint == "" {
		return 0, nil
	}
	var count int
	err := s.p.QueryRow(ctx,
		`UPDATE feedback_reports
		 SET count = count + 1, last_seen = now()
		 WHERE fingerprint = $1
		 RETURNING count`, fingerprint).Scan(&count)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return count, err
}

// SaveProgress upserts the playback position for (user, item).
// Idempotent — repeated calls just bump the position and updated_at.
// `durationSec` is the player's view of the whole movie length; we keep
// it so a future "Continue watching" UI can show progress percentages
// without re-fetching the catalogue.
func (s *Store) SaveProgress(ctx context.Context, userID, itemID string, positionSec, durationSec int) error {
	if s == nil || s.p == nil || userID == "" || itemID == "" {
		return nil
	}
	if positionSec < 0 {
		positionSec = 0
	}
	_, err := s.p.Exec(ctx,
		`INSERT INTO playback_progress
		   (user_id, item_id, position_sec, duration_sec, updated_at)
		 VALUES ($1, $2, $3, $4, now())
		 ON CONFLICT (user_id, item_id) DO UPDATE
		   SET position_sec = EXCLUDED.position_sec,
		       duration_sec = GREATEST(EXCLUDED.duration_sec, playback_progress.duration_sec),
		       updated_at   = now()`,
		userID, itemID, positionSec, durationSec)
	return err
}
