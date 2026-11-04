package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/store"
)

// getPreferences -> GET /me/preferences: what the caller has set for their
// account, the defaults where they have set nothing.
func getPreferences(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		p, err := st.GetPreferences(r.Context(), userID)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}

// patchPreferences -> PATCH /me/preferences: the fields in the body change,
// the others stay, and the answer is the whole document. A key it does not
// know is ignored, as a client from a newer version may send one.
func patchPreferences(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		defer r.Body.Close()
		var body struct {
			WatchlistByKind *bool `json:"watchlist_by_kind"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		p, err := st.PatchPreferences(r.Context(), userID, store.PreferencesPatch{WatchlistByKind: body.WatchlistByKind})
		if errors.Is(err, store.ErrNoDatabase) {
			http.Error(w, "preferences unavailable", http.StatusServiceUnavailable)
			return
		}
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}
