package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ErrNoDatabase is a write to a Store without a database, which keeps nothing.
var ErrNoDatabase = errors.New("no database")

// Preferences is what a person has set for their account.
type Preferences struct {
	// WatchlistByKind splits every list into a movies row and a shows row.
	WatchlistByKind bool `json:"watchlist_by_kind"`
}

// PreferencesPatch is what a write changes: a nil field stays as it is.
type PreferencesPatch struct {
	WatchlistByKind *bool
}

// GetPreferences is userID's preferences: the defaults for a person who has
// set none, and without a database.
func (s *Store) GetPreferences(ctx context.Context, userID string) (Preferences, error) {
	var p Preferences
	if s == nil || s.p == nil || userID == "" {
		return p, nil
	}
	err := s.p.QueryRow(ctx,
		`SELECT watchlist_by_kind FROM user_preferences WHERE user_id = $1`, userID).
		Scan(&p.WatchlistByKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return Preferences{}, nil
	}
	return p, err
}

// PatchPreferences applies patch to userID's preferences and answers all of
// them. A patch that changes nothing writes nothing.
func (s *Store) PatchPreferences(ctx context.Context, userID string, patch PreferencesPatch) (Preferences, error) {
	if patch.WatchlistByKind == nil {
		return s.GetPreferences(ctx, userID)
	}
	var p Preferences
	if s == nil || s.p == nil || userID == "" {
		return p, ErrNoDatabase
	}
	err := s.p.QueryRow(ctx,
		`INSERT INTO user_preferences (user_id, watchlist_by_kind) VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE SET watchlist_by_kind = EXCLUDED.watchlist_by_kind, updated_at = now()
		 RETURNING watchlist_by_kind`,
		userID, *patch.WatchlistByKind).Scan(&p.WatchlistByKind)
	return p, err
}
