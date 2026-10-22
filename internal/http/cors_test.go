package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/auth"
)

// preflight is what a browser sends before a cross-origin GET with a Range
// header: no credential, the method and the header it wants to send.
var preflight = http.Header{
	"Origin":                         {"https://receiver.example"},
	"Access-Control-Request-Method":  {"GET"},
	"Access-Control-Request-Headers": {"range"},
}

// A web player on another origin, a Cast receiver among them, plays from the
// media routes: every answer allows any origin, the 401 of a request without
// a credential too, and a preflight is answered without asking for one or
// asking chino-stream. chino-stream's own CORS header is not passed on beside
// chino-api's: a browser refuses an answer with two.
func TestMediaRoutesAreOpenToOtherOrigins(t *testing.T) {
	var upstream atomic.Int32
	stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstream.Add(1)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "text/vtt")
		_, _ = w.Write([]byte("WEBVTT\n"))
	}))
	t.Cleanup(stream.Close)
	h := streamRouter(t, stream.URL)
	tok := streamToken(t)
	origin := http.Header{"Origin": {"https://receiver.example"}}

	for _, path := range []string{
		"/api/v1/items/i1/play/master.m3u8",
		"/api/v1/items/i1/play/v0/seg-00001.m4s",
		"/api/v1/items/i1/play/high/3.m4s",
		"/api/v1/items/i1/play/s0/seg-00001.vtt",
		"/api/v1/items/i1/play/subtitles/2.vtt",
		"/api/v1/play/subs/sub1.vtt",
		"/api/v1/items/i1/extras/" + testExtra + "/play/master.m3u8",
	} {
		w := do(h, "GET", path+"?stream="+tok, origin)
		if got := w.Header().Values("Access-Control-Allow-Origin"); w.Code != http.StatusOK || len(got) != 1 || got[0] != "*" {
			t.Errorf("GET %s: %d, Access-Control-Allow-Origin %q", path, w.Code, got)
		}
		w = do(h, "GET", path, origin)
		if got := w.Header().Get("Access-Control-Allow-Origin"); w.Code != http.StatusUnauthorized || got != "*" {
			t.Errorf("GET %s without a credential: %d, Access-Control-Allow-Origin %q", path, w.Code, got)
		}
	}

	before := upstream.Load()
	for _, path := range []string{
		"/api/v1/items/i1/play/master.m3u8?stream=" + tok,
		"/api/v1/items/i1/play/v0/seg-00001.m4s",
		"/api/v1/items/i1/play/audio/1/index.m3u8",
		"/api/v1/items/i1/play/trickplay/sprite-3.jpg",
		"/api/v1/items/i1/poster",
		"/api/v1/people/p1/profile",
		"/api/v1/play/subs/sub1.vtt",
		"/api/v1/items/i1/extras/" + testExtra + "/play/v0/seg-00001.m4s",
	} {
		w := do(h, "OPTIONS", path, preflight)
		got := w.Header()
		if w.Code != http.StatusNoContent || got.Get("Access-Control-Allow-Origin") != "*" ||
			got.Get("Access-Control-Allow-Methods") != "GET, OPTIONS" || got.Get("Access-Control-Allow-Headers") != "Range" {
			t.Errorf("preflight %s: %d %v", path, w.Code, got)
		}
	}
	if n := upstream.Load() - before; n != 0 {
		t.Errorf("%d upstream requests for preflights", n)
	}
}

// A preflight names no title: it is answered alike for a title a cap does not
// allow, an unrated one, one there is not and one the cap allows, with a
// capped credential or none, and nothing is asked upstream, not even what the
// cap allows.
func TestAPreflightSaysNothingOfATitle(t *testing.T) {
	is := newIssuer(t)
	h, katalog, stream, manager := ratedRouter(t, is)
	upstream := func() []string { return append(append(stream.take(), manager.take()...), katalog.take()...) }
	signer, err := auth.NewSigner(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	streamTok, _ := signer.MintCapped("kid-1", 12, time.Hour)
	capped := bearerWith(t, is, "kid-1", 12)

	var routes []string
	err = chi.Walk(h.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodOptions && (strings.Contains(route, "{id}") || strings.HasPrefix(route, "/api/v1/play/subs/")) &&
			!strings.HasPrefix(route, "/api/v1/people/") {
			routes = append(routes, route)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) < 20 {
		t.Fatalf("%d preflight routes of one title, want the media routes (20 and more)", len(routes))
	}
	withBearer := http.Header{"Authorization": capped["Authorization"]}
	for k, v := range preflight {
		withBearer[k] = v
	}
	upstream()
	for _, route := range routes {
		for _, title := range []string{"adult", "unrated", "no-such-title", "kid"} {
			path := pathOf(route, title)
			for _, c := range []struct {
				query  string
				header http.Header
			}{{"", preflight}, {"", withBearer}, {"?stream=" + streamTok, preflight}} {
				w := do(h, "OPTIONS", path+c.query, c.header)
				if w.Code != http.StatusNoContent || w.Body.Len() != 0 || w.Header().Get("Access-Control-Allow-Origin") != "*" {
					t.Errorf("preflight %s%s: %d %q", path, c.query, w.Code, w.Body)
				}
				if got := upstream(); len(got) != 0 {
					t.Errorf("preflight %s%s went upstream: %q", path, c.query, got)
				}
			}
		}
	}
}

// The routes that take a bearer in a header stay closed to other origins: no
// CORS header on their answers and no preflight answered. Nor is one for the
// media group's POST, the prewarm, or for the telemetry sink.
func TestSignedInRoutesStayClosedToOtherOrigins(t *testing.T) {
	h := streamRouter(t, "http://127.0.0.1:1")
	for _, path := range []string{
		"/api/v1/me",
		"/api/v1/items",
		"/api/v1/me/stream-token",
		"/api/v1/items/i1/progress",
		"/api/v1/items/i1/play/prewarm",
		"/api/v1/play/events",
	} {
		w := do(h, "OPTIONS", path, preflight)
		if w.Code < 300 || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("preflight %s: %d %v", path, w.Code, w.Header())
		}
	}
	w := do(h, "GET", "/api/v1/me", http.Header{"Origin": {"https://receiver.example"}})
	if w.Code != http.StatusUnauthorized || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("GET /api/v1/me: %d %v", w.Code, w.Header())
	}
}
