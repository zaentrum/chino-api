package http

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/katalog"
	"github.com/zaentrum/chino-api/internal/store"
)

// searchPeople proxies cast/crew name search. `?q=` required (empty →
// empty list), `?limit=` clamps (default 20, max 50). Powers the "Cast &
// crew" section of search and the "search an actor → see their films"
// flow.
func searchPeople(kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		limit := 20
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 50 {
				limit = n
			}
		}
		people, err := kc.SearchPeople(r.Context(), bearerFrom(r), q, limit)
		if err != nil {
			http.Error(w, "katalog: "+err.Error(), http.StatusBadGateway)
			return
		}
		if people == nil {
			people = []katalog.Person{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"people": people, "total": len(people)})
	}
}

// getPerson proxies a person + their filmography. The filmography items
// are watched-stamped for the current user so the grid shows the watched
// badge (poster URLs are already synthesised by the katalog client).
// 404 when the person id doesn't exist.
func getPerson(st *store.Store, kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
				limit = n
			}
		}
		pd, err := kc.GetPerson(r.Context(), bearerFrom(r), id, limit)
		if err != nil {
			http.Error(w, "katalog: "+err.Error(), http.StatusBadGateway)
			return
		}
		if pd == nil {
			http.Error(w, "person not found", http.StatusNotFound)
			return
		}
		userID, _ := auth.SubjectFromContext(r.Context())
		stampWatchedSlice(r.Context(), st, userID, pd.Items)
		writeJSON(w, http.StatusOK, pd)
	}
}
