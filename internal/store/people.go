package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// A person's data, deleted: what chino-api keeps of a signed-in person, by
// their subject — playback progress, watch history, named watchlists and
// their items, likes, and the legacy single-watchlist rows. The bug-report
// ledger (feedback_reports) holds no person's data: a fingerprint and a work
// package.
//
// The rows go inside a transaction the caller commits once the person's
// account is gone too, or rolls back when it is not — so a refused account
// deletion deletes nothing, and a retry after a failure starts from what was
// there.

// Deleted counts what a deletion took.
type Deleted struct {
	Progress        int64 `json:"progress"`
	Watched         int64 `json:"watched"`
	Watchlists      int64 `json:"watchlists"`
	WatchlistItems  int64 `json:"watchlistItems"`
	Likes           int64 `json:"likes"`
	LegacyWatchlist int64 `json:"legacyWatchlist"`
}

// Deletion is a person's data, deleted in a transaction not yet committed.
type Deletion struct {
	tx      pgx.Tx
	Deleted Deleted
}

// DeleteUserData deletes the rows of userID and answers the open deletion.
// A nil Store — no database — has nothing to delete, and its deletion
// commits as a no-op.
func (s *Store) DeleteUserData(ctx context.Context, userID string) (*Deletion, error) {
	if userID == "" {
		return nil, errors.New("no subject")
	}
	if s == nil || s.p == nil {
		return &Deletion{}, nil
	}
	tx, err := s.p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	d := &Deletion{tx: tx}
	steps := []struct {
		count *int64
		sql   string
	}{
		{&d.Deleted.WatchlistItems, `DELETE FROM watchlist_items WHERE list_id IN (SELECT id FROM watchlists WHERE user_id = $1)`},
		{&d.Deleted.Watchlists, `DELETE FROM watchlists WHERE user_id = $1`},
		{&d.Deleted.Progress, `DELETE FROM playback_progress WHERE user_id = $1`},
		{&d.Deleted.Watched, `DELETE FROM watched_history WHERE user_id = $1`},
		{&d.Deleted.Likes, `DELETE FROM likes WHERE user_id = $1`},
		{&d.Deleted.LegacyWatchlist, `DELETE FROM watchlist WHERE user_id = $1`},
	}
	for _, step := range steps {
		tag, err := tx.Exec(ctx, step.sql, userID)
		if err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
		*step.count = tag.RowsAffected()
	}
	return d, nil
}

// Commit makes the deletion stick.
func (d *Deletion) Commit(ctx context.Context) error {
	if d == nil || d.tx == nil {
		return nil
	}
	return d.tx.Commit(ctx)
}

// Rollback puts the rows back, as if nothing was asked.
func (d *Deletion) Rollback(ctx context.Context) error {
	if d == nil || d.tx == nil {
		return nil
	}
	return d.tx.Rollback(ctx)
}
