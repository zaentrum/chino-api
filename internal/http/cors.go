package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// mediaCORS opens the media routes (the stream-token group: playlists,
// segments, subtitles, artwork) to web players on other origins. A Cast
// receiver is such a player: a web page served from Google's origin that
// fetches a title's playlists and segments from these URLs, and its browser
// hands it an answer only when the answer allows its origin. Every answer of
// the group allows it, a 401 or a 404 too, so such a player can tell why a
// request failed.
//
// Any origin may read them: these routes take their credential from the URL
// (?stream=, or the bearer in ?token=), never from a cookie, so no other site
// can use a viewer's session through them, and a URL with a valid credential
// plays anywhere already, in an app, a download tool or a <video> element.
// The routes that take a bearer in a header stay closed to other origins.
//
// A browser's preflight (OPTIONS) is answered here, before the credential is
// checked, as a browser sends one without it: GET, and the Range header.
func mediaCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			h.Set("Allow", "GET, OPTIONS")
			h.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Range")
			h.Set("Access-Control-Max-Age", "7200")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// corsGroup is the media group's router: beside each GET route it registers
// the OPTIONS route a preflight is sent to. chi runs a group's middleware only
// for a route it matched, so without them a preflight would be answered 405
// before mediaCORS saw it. The group's POST, the prewarm, gets none: only the
// app's own pages send it.
type corsGroup struct{ chi.Router }

func (g corsGroup) With(middlewares ...func(http.Handler) http.Handler) chi.Router {
	return corsGroup{g.Router.With(middlewares...)}
}

func (g corsGroup) Get(pattern string, h http.HandlerFunc) {
	g.Router.Get(pattern, h)
	// Never served: mediaCORS answers every OPTIONS of the group.
	g.Router.Options(pattern, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}
