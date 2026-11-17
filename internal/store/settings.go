package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// The settings an account keeps (migration 006): what goes with the account
// to every device it signs in on, by the person's subject. Every other
// setting stays on the device. A person without a row has each setting at
// its first value. The rows go with the rest of the person's data
// (DeleteUserData).

// Settings are an account's settings, as the store keeps them.
type Settings struct {
	// WatchlistByKind is whether every list shows its movies and its shows
	// apart; false at first.
	WatchlistByKind bool
}

// SettingsChange is what one request changes of an account's settings: a
// field left nil stays as it is.
type SettingsChange struct {
	WatchlistByKind *bool
}

// GetSettings is userID's settings: the first values for a person who has
// set none, and with no database.
func (s *Store) GetSettings(ctx context.Context, userID string) (Settings, error) {
	if s == nil || s.p == nil || userID == "" {
		return Settings{}, nil
	}
	var out Settings
	err := s.p.QueryRow(ctx,
		`SELECT watchlist_by_kind FROM account_settings WHERE user_id = $1`,
		userID).Scan(&out.WatchlistByKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, err
	}
	return out, nil
}

// ChangeSettings writes change for userID, in one statement, and answers the
// settings as kept after it; a change of nothing reads them. With no
// database nothing is kept, and the answer is the first values. A change,
// one of nothing too, is ErrAccountDeleted once the account's deletion has
// begun (asAccount).
func (s *Store) ChangeSettings(ctx context.Context, userID string, change SettingsChange) (Settings, error) {
	if s == nil || s.p == nil || userID == "" {
		return s.GetSettings(ctx, userID)
	}
	var out Settings
	err := s.asAccount(ctx, userID, func(tx pgx.Tx) error {
		if change.WatchlistByKind == nil {
			err := tx.QueryRow(ctx,
				`SELECT watchlist_by_kind FROM account_settings WHERE user_id = $1`,
				userID).Scan(&out.WatchlistByKind)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO account_settings (user_id, watchlist_by_kind, updated_at)
			 VALUES ($1, $2, now())
			 ON CONFLICT (user_id) DO UPDATE
			   SET watchlist_by_kind = EXCLUDED.watchlist_by_kind,
			       updated_at        = now()
			 RETURNING watchlist_by_kind`,
			userID, *change.WatchlistByKind).Scan(&out.WatchlistByKind)
	})
	if err != nil {
		return Settings{}, err
	}
	return out, nil
}
