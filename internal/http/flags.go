package http

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/store"
)

// flagsHandlers covers the watchlist + likes "user flag" tables. The
// shape is identical for both — only the table name differs — so we
// generate handlers from a small descriptor instead of duplicating four
// near-identical functions.

type flagSpec struct {
	table string // store.WatchlistTable | store.LikesTable
	field string // JSON key under which the boolean lives ("watchlist", "liked")
}

// list returns the user's current set of items in the flag table,
// newest-first. The body is a thin envelope so the front-end can read
// `items` regardless of which flag it's asking about — mirrors the
// shape of /continue-watching.
func flagList(st *store.Store, spec flagSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		ids, err := st.ListFlag(r.Context(), spec.table, userID, 200)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": idsOrEmpty(ids)})
	}
}

// set toggles an item's membership in a flag table. PUT = add, DELETE =
// remove. Both idempotent. Return 204 either way so the client can
// update its local state without parsing a body.
func flagSet(st *store.Store, spec flagSpec, present bool) http.HandlerFunc {
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
		if err := st.SetFlag(r.Context(), spec.table, userID, itemID, present); err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// idsOrEmpty exists so the JSON encoder emits `[]` for an empty result
// instead of `null` — the chino-web hook treats null as "not loaded
// yet" which would mis-render the empty state.
func idsOrEmpty(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}
