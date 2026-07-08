package http

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/katalog"
	"github.com/zaentrum/chino-api/internal/store"
)

// postWatched marks an item as fully watched for the current user.
// Called by the player when the user enters the credits segment OR the
// position crosses 95 % of duration (whichever fires first), and by the
// chino-web watched toggle on DetailPage / EpisodesList. The resulting
// watched_history row drives the "Watched" pill on MediaCard and lets
// continue-watching substitute the next episode for finished rows.
//
// Idempotent: re-watching just bumps watched_at.
func postWatched(st *store.Store) http.HandlerFunc {
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
		if err := st.MarkWatched(r.Context(), userID, itemID); err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// listWatched returns the current user's watch history newest-first,
// each entry resolved against katalog so the client can render a
// MediaCard without a follow-up fetch. Mirrors the parallel-resolve
// pattern continueWatching uses (worker pool with cap=6, in-place
// compaction). Paginated via ?limit + ?offset for heavy watchers.
type watchedItem struct {
	katalog.Item
	WatchedAt string `json:"watched_at"`
}

func listWatched(st *store.Store, kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		bearer := bearerFrom(r)
		limit := 60
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
				limit = n
			}
		}
		offset := 0
		if v := r.URL.Query().Get("offset"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				offset = n
			}
		}
		rows, err := st.ListWatched(r.Context(), userID, limit, offset)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}

		out := make([]watchedItem, len(rows))
		ok := make([]bool, len(rows))
		var wg sync.WaitGroup
		sem := make(chan struct{}, 6)
		for i, row := range rows {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, row store.WatchedRow) {
				defer wg.Done()
				defer func() { <-sem }()
				item, err := kc.GetItem(r.Context(), bearer, row.ItemID)
				if err != nil || item == nil {
					return
				}
				out[i] = watchedItem{Item: *item, WatchedAt: row.WatchedAt.Format("2006-01-02T15:04:05Z07:00")}
				ok[i] = true
			}(i, row)
		}
		wg.Wait()

		// Compact in-place, preserving the newest-first order.
		compact := out[:0]
		for i, valid := range ok {
			if valid {
				compact = append(compact, out[i])
			}
		}
		// Stamp artwork ?stream= tokens (this list is built outside stampWatched).
		ptrs := make([]*katalog.Item, len(compact))
		for i := range compact {
			ptrs[i] = &compact[i].Item
		}
		stampArtworkTokens(userID, ptrs)
		writeJSON(w, http.StatusOK, map[string]any{"items": compact})
	}
}

// deleteWatched clears the watched_history row for an item. Counterpart
// to postWatched — used by the chino-web toggle so the user can unmark
// something they accidentally finished or want to re-watch fresh.
func deleteWatched(st *store.Store) http.HandlerFunc {
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
		if err := st.UnmarkWatched(r.Context(), userID, itemID); err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
