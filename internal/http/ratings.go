package http

import (
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/katalog"
)

// A capped viewer (auth.MaxRatingFromContext: its bearer's max_rating, or the
// cap of its stream token) is served only the titles its cap allows.
// katalog-api decides what that is: on every catalog request chino-api makes
// for the viewer (the katalog client passes the cap), and through
// /api/v1/visible for what chino-api serves or proxies without asking the
// catalog. A title the cap does not allow is as one there is not: every route
// of one title answers it 404, before anything goes upstream, so the answer
// does not say the title exists. An uncapped viewer is served as before.

// gate holds a capped viewer's requests for one title to what its cap
// allows.
type gate struct{ kc *katalog.Client }

// title is middleware for a route of one title, which the URL parameter
// param names: a capped viewer is answered 404 for a title its cap does not
// allow, and for an id that names none, as katalog-api answers both. When
// katalog-api cannot say, the answer is 502 and nothing is served.
func (g gate) title(param string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			age, capped := auth.MaxRatingFromContext(r.Context())
			if !capped {
				next.ServeHTTP(w, r)
				return
			}
			g.serve(w, r, next, age, chi.URLParam(r, param))
		})
	}
}

// subtitle is middleware for a route of one sidecar subtitle (the URL
// parameter id): a capped viewer is answered 404 for a subtitle of a title
// its cap does not allow, and for one there is not.
func (g gate) subtitle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		age, capped := auth.MaxRatingFromContext(r.Context())
		if !capped {
			next.ServeHTTP(w, r)
			return
		}
		item, err := g.kc.SubtitleItem(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			log.Printf("chino-api: the title of a subtitle could not be looked up: %v", err)
			http.Error(w, "catalog unavailable", http.StatusBadGateway)
			return
		}
		g.serve(w, r, next, age, item)
	})
}

// serve serves r with next when a viewer capped at age may be served the
// title id, and answers 404 otherwise.
func (g gate) serve(w http.ResponseWriter, r *http.Request, next http.Handler, age int, id string) {
	ok := false
	if id != "" {
		var err error
		if ok, err = g.kc.VisibleOne(r.Context(), age, id); err != nil {
			log.Printf("chino-api: what a rating cap allows could not be asked: %v", err)
			http.Error(w, "catalog unavailable", http.StatusBadGateway)
			return
		}
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	next.ServeHTTP(w, r)
}
