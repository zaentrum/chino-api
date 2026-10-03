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

// getPerson proxies a person, their details and their filmography. The
// filmography items are watched-stamped for the current user so the grid
// shows the watched badge (poster URLs, and the person's profile_url, are
// synthesised by the katalog client). ?lang= and Accept-Language go on to
// katalog-api, which picks the biography's language from them. 404 when the
// person id doesn't exist.
func getPerson(st *store.Store, kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
				limit = n
			}
		}
		pd, err := kc.GetPerson(r.Context(), bearerFrom(r), id, limit,
			r.URL.Query().Get("lang"), r.Header.Get("Accept-Language"))
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
		// The biography's language follows the header when ?lang= is absent.
		w.Header().Add("Vary", "Accept-Language")
		writeJSON(w, http.StatusOK, pd)
	}
}

// proxyPersonProfile streams a person's portrait from katalog-manager
// (/api/artwork/person/{id}/profile), as proxyArtwork streams an item's
// poster. It sits in the stream-token group with the posters, so a
// profile_url in an <img src> authenticates with ?stream= (or ?token=),
// which ProxyStream passes on with the query. Range, If-None-Match and the
// upstream's status and headers (ETag, 304, 404) pass through.
func proxyPersonProfile(kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		kc.ProxyStream(w, r, "/api/artwork/person/"+id+"/profile", bearerFrom(r))
	}
}
