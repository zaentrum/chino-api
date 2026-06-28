package store

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Multiple named watchlists. The model is two tables (see migration 005):
// `watchlists` holds one row per (user, list) with exactly one default
// list per user, and `watchlist_items` holds membership. Everything here
// keeps the same nil-store-graceful contract as store.go — a nil Store /
// empty userID degrades quietly so the no-DB development path keeps
// working — with one exception: the mutating helpers that enforce
// invariants (create/rename/delete) return typed sentinel errors the
// handler maps to 409/400/404.

// Sentinel errors surfaced to the HTTP layer. The handler switches on
// these to pick the right status code instead of leaking SQL detail.
var (
	ErrListLimit           = errors.New("too many lists")
	ErrDuplicateName       = errors.New("name exists")
	ErrCannotDeleteDefault = errors.New("cannot delete default")
	ErrListNotFound        = errors.New("list not found")
)

// maxListsPerUser caps how many lists a single subject may own. Matches
// the frozen API contract (50 → 409 "too many lists").
const maxListsPerUser = 50

// defaultListName is the name every user's lazily-created default list
// carries. The contract pins this exact string.
const defaultListName = "Watchlist"

// WatchlistRow is one list in the overview, with its current item count.
// CreatedAt drives the "default first, then others by createdAt asc"
// ordering the contract requires.
type WatchlistRow struct {
	ID        string
	Name      string
	IsDefault bool
	CreatedAt time.Time
	ItemCount int
}

// defaultListID is the deterministic id for a user's default list. It
// must match migration 005 ('dflt_' || left(md5(user_id), 24)) so a user
// who pre-existed the migration and one created fresh both resolve to the
// same single default row, guarded by the partial unique index.
func defaultListID(userID string) string {
	sum := md5.Sum([]byte(userID))
	return "dflt_" + hex.EncodeToString(sum[:])[:24]
}

// newListID mints a random 16-byte hex id for a non-default list. No
// uuid lib in go.mod, so crypto/rand hex per the project's house style.
func newListID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "wl_" + hex.EncodeToString(b[:])
}

// EnsureDefaultList returns the id of the user's default list, creating
// it lazily on first access. Nil store / empty user → "" with no error.
func (s *Store) EnsureDefaultList(ctx context.Context, userID string) (string, error) {
	if s == nil || s.p == nil || userID == "" {
		return "", nil
	}
	id := defaultListID(userID)
	_, err := s.p.Exec(ctx,
		`INSERT INTO watchlists (id, user_id, name, is_default, created_at)
		 VALUES ($1, $2, $3, true, now())
		 ON CONFLICT (id) DO NOTHING`,
		id, userID, defaultListName)
	if err != nil {
		return "", err
	}
	return id, nil
}

// ListWatchlists returns the user's lists, default first then the rest by
// createdAt ascending, each with its current item count. Ensures the
// default exists first so even a brand-new user gets exactly one row.
func (s *Store) ListWatchlists(ctx context.Context, userID string) ([]WatchlistRow, error) {
	if s == nil || s.p == nil || userID == "" {
		return nil, nil
	}
	if _, err := s.EnsureDefaultList(ctx, userID); err != nil {
		return nil, err
	}
	rows, err := s.p.Query(ctx,
		`SELECT w.id, w.name, w.is_default, w.created_at,
		        COUNT(i.item_id) AS item_count
		 FROM watchlists w
		 LEFT JOIN watchlist_items i ON i.list_id = w.id
		 WHERE w.user_id = $1
		 GROUP BY w.id, w.name, w.is_default, w.created_at
		 ORDER BY w.is_default DESC, w.created_at ASC`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WatchlistRow
	for rows.Next() {
		var r WatchlistRow
		if err := rows.Scan(&r.ID, &r.Name, &r.IsDefault, &r.CreatedAt, &r.ItemCount); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetWatchlist returns a single list's metadata (id/name/is_default), or
// ErrListNotFound when it isn't the caller's. The item list is fetched
// separately via GetWatchlistItems.
func (s *Store) GetWatchlist(ctx context.Context, userID, listID string) (WatchlistRow, error) {
	if s == nil || s.p == nil || userID == "" || listID == "" {
		return WatchlistRow{}, ErrListNotFound
	}
	var r WatchlistRow
	err := s.p.QueryRow(ctx,
		`SELECT id, name, is_default, created_at
		 FROM watchlists WHERE id = $1 AND user_id = $2`,
		listID, userID).Scan(&r.ID, &r.Name, &r.IsDefault, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return WatchlistRow{}, ErrListNotFound
	}
	if err != nil {
		return WatchlistRow{}, err
	}
	return r, nil
}

// CreateWatchlist makes a new (non-default) list after validating the
// per-user cap and case-insensitive name uniqueness. Name is assumed
// already trimmed/length-checked by the handler. Returns the new row.
func (s *Store) CreateWatchlist(ctx context.Context, userID, name string) (WatchlistRow, error) {
	if s == nil || s.p == nil || userID == "" {
		return WatchlistRow{}, ErrListNotFound
	}
	// Make sure the default exists so it counts toward the cap and the
	// user always has at least one list.
	if _, err := s.EnsureDefaultList(ctx, userID); err != nil {
		return WatchlistRow{}, err
	}
	var count int
	if err := s.p.QueryRow(ctx,
		`SELECT COUNT(*) FROM watchlists WHERE user_id = $1`, userID).Scan(&count); err != nil {
		return WatchlistRow{}, err
	}
	if count >= maxListsPerUser {
		return WatchlistRow{}, ErrListLimit
	}
	dup, err := s.nameExists(ctx, userID, name, "")
	if err != nil {
		return WatchlistRow{}, err
	}
	if dup {
		return WatchlistRow{}, ErrDuplicateName
	}
	r := WatchlistRow{ID: newListID(), Name: name, IsDefault: false}
	err = s.p.QueryRow(ctx,
		`INSERT INTO watchlists (id, user_id, name, is_default, created_at)
		 VALUES ($1, $2, $3, false, now())
		 RETURNING created_at`,
		r.ID, userID, name).Scan(&r.CreatedAt)
	if err != nil {
		return WatchlistRow{}, err
	}
	return r, nil
}

// RenameWatchlist renames the caller's list (default included — the
// is_default flag is untouched). Enforces the same name validation as
// create. ErrListNotFound when it isn't the caller's.
func (s *Store) RenameWatchlist(ctx context.Context, userID, listID, name string) (WatchlistRow, error) {
	if s == nil || s.p == nil || userID == "" || listID == "" {
		return WatchlistRow{}, ErrListNotFound
	}
	// Confirm ownership first so a duplicate-name 409 can't leak the
	// existence of someone else's list.
	cur, err := s.GetWatchlist(ctx, userID, listID)
	if err != nil {
		return WatchlistRow{}, err
	}
	dup, err := s.nameExists(ctx, userID, name, listID)
	if err != nil {
		return WatchlistRow{}, err
	}
	if dup {
		return WatchlistRow{}, ErrDuplicateName
	}
	_, err = s.p.Exec(ctx,
		`UPDATE watchlists SET name = $1 WHERE id = $2 AND user_id = $3`,
		name, listID, userID)
	if err != nil {
		return WatchlistRow{}, err
	}
	cur.Name = name
	return cur, nil
}

// DeleteWatchlist removes a list and (via ON DELETE CASCADE) its items.
// The default list can't be deleted. ErrListNotFound when it isn't the
// caller's; ErrCannotDeleteDefault when targeting the default.
func (s *Store) DeleteWatchlist(ctx context.Context, userID, listID string) error {
	if s == nil || s.p == nil || userID == "" || listID == "" {
		return ErrListNotFound
	}
	cur, err := s.GetWatchlist(ctx, userID, listID)
	if err != nil {
		return err
	}
	if cur.IsDefault {
		return ErrCannotDeleteDefault
	}
	_, err = s.p.Exec(ctx,
		`DELETE FROM watchlists WHERE id = $1 AND user_id = $2`, listID, userID)
	return err
}

// GetWatchlistItems returns the item ids in a list, newest-added first.
// Caller is expected to have already confirmed ownership (e.g. via
// GetWatchlist). Nil store → nil.
func (s *Store) GetWatchlistItems(ctx context.Context, listID string) ([]string, error) {
	if s == nil || s.p == nil || listID == "" {
		return nil, nil
	}
	rows, err := s.p.Query(ctx,
		`SELECT item_id FROM watchlist_items
		 WHERE list_id = $1 ORDER BY added_at DESC`, listID)
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

// AddWatchlistItem idempotently adds an item to one of the caller's
// lists. ErrListNotFound when the list isn't the caller's. Idempotent —
// re-adding is a no-op.
func (s *Store) AddWatchlistItem(ctx context.Context, userID, listID, itemID string) error {
	if s == nil || s.p == nil || userID == "" || listID == "" || itemID == "" {
		return nil
	}
	owns, err := s.ownsList(ctx, userID, listID)
	if err != nil {
		return err
	}
	if !owns {
		return ErrListNotFound
	}
	_, err = s.p.Exec(ctx,
		`INSERT INTO watchlist_items (list_id, item_id, added_at)
		 VALUES ($1, $2, now())
		 ON CONFLICT (list_id, item_id) DO NOTHING`,
		listID, itemID)
	return err
}

// RemoveWatchlistItem idempotently drops an item from one of the caller's
// lists. ErrListNotFound when the list isn't the caller's. Deleting an
// absent membership is a no-op.
func (s *Store) RemoveWatchlistItem(ctx context.Context, userID, listID, itemID string) error {
	if s == nil || s.p == nil || userID == "" || listID == "" || itemID == "" {
		return nil
	}
	owns, err := s.ownsList(ctx, userID, listID)
	if err != nil {
		return err
	}
	if !owns {
		return ErrListNotFound
	}
	_, err = s.p.Exec(ctx,
		`DELETE FROM watchlist_items WHERE list_id = $1 AND item_id = $2`,
		listID, itemID)
	return err
}

// ListMemberships maps each requested item id to the list ids (among the
// caller's lists) it belongs to. Items in no list are omitted. Powers the
// add-to-list picker checkmarks and the card "saved" badge in one
// round-trip. Nil store / empty input → empty map.
func (s *Store) ListMemberships(ctx context.Context, userID string, itemIDs []string) (map[string][]string, error) {
	out := make(map[string][]string)
	if s == nil || s.p == nil || userID == "" || len(itemIDs) == 0 {
		return out, nil
	}
	rows, err := s.p.Query(ctx,
		`SELECT i.item_id, i.list_id
		 FROM watchlist_items i
		 JOIN watchlists w ON w.id = i.list_id
		 WHERE w.user_id = $1 AND i.item_id = ANY($2)`,
		userID, itemIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var itemID, listID string
		if err := rows.Scan(&itemID, &listID); err != nil {
			return nil, err
		}
		out[itemID] = append(out[itemID], listID)
	}
	return out, rows.Err()
}

// ownsList reports whether listID belongs to userID. The ownership-check
// helper every mutating items endpoint goes through.
func (s *Store) ownsList(ctx context.Context, userID, listID string) (bool, error) {
	var exists bool
	err := s.p.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM watchlists WHERE id = $1 AND user_id = $2)`,
		listID, userID).Scan(&exists)
	return exists, err
}

// nameExists reports whether the user already has a list whose name
// matches case-insensitively. excludeID skips a list id (used by rename
// so renaming to the same name with different casing isn't a false dup).
func (s *Store) nameExists(ctx context.Context, userID, name, excludeID string) (bool, error) {
	var exists bool
	err := s.p.QueryRow(ctx,
		`SELECT EXISTS(
		   SELECT 1 FROM watchlists
		   WHERE user_id = $1 AND lower(name) = lower($2) AND id <> $3)`,
		userID, strings.TrimSpace(name), excludeID).Scan(&exists)
	return exists, err
}
