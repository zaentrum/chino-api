package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/store"
)

// Multiple named watchlists. These handlers own /me/watchlists* and
// REPLACE the three legacy /me/watchlist* routes (which now operate on
// the caller's default list via EnsureDefaultList, keeping their exact
// {items:[...]} / 204 shapes for already-installed mobile/TV builds).
// /me/likes stays on the old flagSpec path — untouched.

// maxListName / minListName bound a list name after trimming. The
// frozen contract pins 1..60 chars.
const (
	minListName     = 1
	maxListName     = 60
	maxMembershipID = 100 // cap on ?ids= CSV length for the picker hydration
)

// watchlistJSON is the wire shape for a list in the overview / create /
// rename responses. itemCount is omitted-as-zero is fine; createdAt is
// RFC3339 per the contract.
type watchlistJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ItemCount int    `json:"itemCount"`
	IsDefault bool   `json:"isDefault"`
	CreatedAt string `json:"createdAt"`
}

func toWatchlistJSON(r store.WatchlistRow) watchlistJSON {
	return watchlistJSON{
		ID:        r.ID,
		Name:      r.Name,
		ItemCount: r.ItemCount,
		IsDefault: r.IsDefault,
		CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// validListName trims and bounds a proposed name. Returns the cleaned
// name and ok=false when it's empty or too long.
func validListName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if len(name) < minListName || len(name) > maxListName {
		return "", false
	}
	return name, true
}

// listError maps a store sentinel to the right status. Returns true when
// it handled (wrote) the error.
func listError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrListNotFound):
		http.Error(w, "list not found", http.StatusNotFound)
	case errors.Is(err, store.ErrListLimit):
		http.Error(w, "too many lists", http.StatusConflict)
	case errors.Is(err, store.ErrDuplicateName):
		http.Error(w, "name exists", http.StatusConflict)
	case errors.Is(err, store.ErrCannotDeleteDefault):
		http.Error(w, "cannot delete default", http.StatusConflict)
	default:
		http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
	}
	return true
}

// listWatchlists -> GET /me/watchlists. Default list first, then others
// by createdAt asc (ordering enforced in the store query).
func listWatchlists(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		rows, err := st.ListWatchlists(r.Context(), userID)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		lists := make([]watchlistJSON, 0, len(rows))
		for _, row := range rows {
			lists = append(lists, toWatchlistJSON(row))
		}
		writeJSON(w, http.StatusOK, map[string]any{"lists": lists})
	}
}

// createWatchlist -> POST /me/watchlists {"name"}. 201 with the new list.
func createWatchlist(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		name, ok := validListName(body.Name)
		if !ok {
			http.Error(w, "name must be 1..60 chars", http.StatusBadRequest)
			return
		}
		row, err := st.CreateWatchlist(r.Context(), userID, name)
		if listError(w, err) {
			return
		}
		writeJSON(w, http.StatusCreated, toWatchlistJSON(row))
	}
}

// renameWatchlist -> PATCH /me/watchlists/{listId} {"name"}. Renaming the
// default keeps isDefault=true (store leaves the flag alone).
func renameWatchlist(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		listID := chi.URLParam(r, "listId")
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		name, ok := validListName(body.Name)
		if !ok {
			http.Error(w, "name must be 1..60 chars", http.StatusBadRequest)
			return
		}
		row, err := st.RenameWatchlist(r.Context(), userID, listID, name)
		if listError(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, toWatchlistJSON(row))
	}
}

// deleteWatchlist -> DELETE /me/watchlists/{listId}. 204; cascades items.
func deleteWatchlist(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		listID := chi.URLParam(r, "listId")
		if err := st.DeleteWatchlist(r.Context(), userID, listID); listError(w, err) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// getWatchlist -> GET /me/watchlists/{listId}. Returns the list metadata
// plus its item ids (newest-added first). 404 if not the caller's.
func getWatchlist(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		listID := chi.URLParam(r, "listId")
		row, err := st.GetWatchlist(r.Context(), userID, listID)
		if listError(w, err) {
			return
		}
		ids, err := st.GetWatchlistItems(r.Context(), listID)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":        row.ID,
			"name":      row.Name,
			"isDefault": row.IsDefault,
			"items":     idsOrEmpty(ids),
		})
	}
}

// setWatchlistItem handles PUT/DELETE /me/watchlists/{listId}/items/{itemId}.
// Both idempotent, 204; 404 if the list isn't the caller's.
func setWatchlistItem(st *store.Store, add bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		listID := chi.URLParam(r, "listId")
		itemID := chi.URLParam(r, "itemId")
		if itemID == "" {
			http.Error(w, "missing item id", http.StatusBadRequest)
			return
		}
		var err error
		if add {
			err = st.AddWatchlistItem(r.Context(), userID, listID, itemID)
		} else {
			err = st.RemoveWatchlistItem(r.Context(), userID, listID, itemID)
		}
		if listError(w, err) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// watchlistMemberships -> GET /me/watchlists/memberships?ids=a,b,c.
// Returns { memberships: { itemId: [listId,...] } } for the caller's
// lists. Items in no list may be omitted. Drives the picker checkmarks
// and the card "saved" badge.
func watchlistMemberships(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		ids := parseIDsCSV(r.URL.Query().Get("ids"), maxMembershipID)
		m, err := st.ListMemberships(r.Context(), userID, ids)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if m == nil {
			m = map[string][]string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"memberships": m})
	}
}

// parseIDsCSV splits a comma-separated id list, trims blanks, dedups, and
// caps at max so a single request can't pull an unbounded membership
// scan.
func parseIDsCSV(raw string, max int) []string {
	if raw == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		if len(out) >= max {
			break
		}
	}
	return out
}

// --- Back-compat: /me/watchlist now maps to the default list ---------------
//
// These keep the exact response shapes the already-installed mobile/TV
// builds expect ({items:[...]} on GET, 204 on PUT/DELETE) but operate on
// the user's default list resolved via EnsureDefaultList. The default
// list is created lazily here so a fresh user's first add still works.

// defaultListGet -> GET /me/watchlist == default list's items.
func defaultListGet(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		listID, err := st.EnsureDefaultList(r.Context(), userID)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		ids, err := st.GetWatchlistItems(r.Context(), listID)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": idsOrEmpty(ids)})
	}
}

// defaultListSet -> PUT/DELETE /me/watchlist/{id} == add/remove on the
// default list. 204 either way.
func defaultListSet(st *store.Store, add bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		itemID := chi.URLParam(r, "id")
		if itemID == "" {
			http.Error(w, "missing item id", http.StatusBadRequest)
			return
		}
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		listID, err := st.EnsureDefaultList(r.Context(), userID)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if add {
			err = st.AddWatchlistItem(r.Context(), userID, listID, itemID)
		} else {
			err = st.RemoveWatchlistItem(r.Context(), userID, listID, itemID)
		}
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
