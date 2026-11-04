package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
)

func patch(h http.Handler, path, body string, header http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	for k, v := range header {
		r.Header[k] = v
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Without a database nothing can be kept: /api/config offers neither feature,
// reading answers the defaults and a write says it was not kept.
func TestPreferencesWithoutADatabase(t *testing.T) {
	is := newIssuer(t)
	h, err := NewRouter(config.Config{OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: "http://katalog.invalid", StreamBaseURL: "http://chino-stream.invalid",
		ArtworkBaseURL: "http://katalog-manager.invalid", StreamSigningKey: signingKey}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	bearer := bearerWith(t, is, "someone", nil)
	if w := do(h, "GET", "/api/v1/me/preferences", http.Header{}); w.Code != http.StatusUnauthorized {
		t.Errorf("no bearer: %d, want 401", w.Code)
	}
	if w := do(h, "GET", "/api/v1/me/preferences", bearer); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"watchlist_by_kind":false}` {
		t.Errorf("GET: %d %s", w.Code, w.Body)
	}
	if w := patch(h, "/api/v1/me/preferences", `{"watchlist_by_kind":true}`, bearer); w.Code != http.StatusServiceUnavailable {
		t.Errorf("PATCH: %d, want 503", w.Code)
	}
	for _, body := range []string{`{`, `{"watchlist_by_kind":"yes"}`} {
		if w := patch(h, "/api/v1/me/preferences", body, bearer); w.Code != http.StatusBadRequest {
			t.Errorf("PATCH %s: %d, want 400", body, w.Code)
		}
	}
	if w := patch(h, "/api/v1/me/preferences", `{"another":1}`, bearer); w.Code != http.StatusOK {
		t.Errorf("PATCH of a key it does not know: %d, want 200", w.Code)
	}
}

// A person's preferences start at the defaults, change by what a write names,
// and are theirs alone.
func TestPreferencesAreAPersonsOwn(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	h, err := NewRouter(config.Config{OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: "http://katalog.invalid", StreamBaseURL: "http://chino-stream.invalid",
		ArtworkBaseURL: "http://katalog-manager.invalid", StreamSigningKey: signingKey}, st, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	one, other := bearerWith(t, is, "one", nil), bearerWith(t, is, "other", nil)
	const off, on = `{"watchlist_by_kind":false}`, `{"watchlist_by_kind":true}`
	check := func(what string, w *httptest.ResponseRecorder, want string) {
		t.Helper()
		if got := strings.TrimSpace(w.Body.String()); w.Code != http.StatusOK || got != want {
			t.Errorf("%s: %d %s, want %s", what, w.Code, got, want)
		}
	}
	check("the default", do(h, "GET", "/api/v1/me/preferences", one), off)
	check("turned on", patch(h, "/api/v1/me/preferences", on, one), on)
	check("read back", do(h, "GET", "/api/v1/me/preferences", one), on)
	check("a patch that names nothing", patch(h, "/api/v1/me/preferences", `{}`, one), on)
	check("another person's", do(h, "GET", "/api/v1/me/preferences", other), off)
	check("turned off", patch(h, "/api/v1/me/preferences", off, one), off)
}

// A list asked for by kind holds that row's titles, an episode among the
// shows; its counts say the same; and only a caller that asks gets counts.
func TestAListByKind(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	katalog := ratedKatalog(t)
	h, err := NewRouter(config.Config{OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: katalog.URL, StreamBaseURL: "http://chino-stream.invalid",
		ArtworkBaseURL: "http://katalog-manager.invalid", StreamSigningKey: signingKey}, st, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	def, err := st.EnsureDefaultList(ctx, "adult-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"adult", "ep6", "kid"} {
		if err := st.AddWatchlistItem(ctx, "adult-1", def, id); err != nil {
			t.Fatal(err)
		}
	}
	type detail struct {
		Items []string `json:"items"`
	}
	bearer := bearerWith(t, is, "adult-1", nil)
	for kind, want := range map[string]int{"movie": 2, "series": 1, "": 3} {
		w := do(h, "GET", "/api/v1/me/watchlists/"+def+"?kind="+kind, bearer)
		var d detail
		if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil || w.Code != http.StatusOK || len(d.Items) != want {
			t.Errorf("kind %q: %d %s, want %d items", kind, w.Code, w.Body, want)
		}
	}
	if w := do(h, "GET", "/api/v1/me/watchlists/"+def+"?kind=album", bearer); w.Code != http.StatusBadRequest {
		t.Errorf("kind album: %d, want 400", w.Code)
	}
	counts := func(path string) string {
		t.Helper()
		var doc struct {
			Lists []struct {
				Counts *watchlistCounts `json:"counts"`
			} `json:"lists"`
		}
		w := do(h, "GET", path, bearer)
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || len(doc.Lists) == 0 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		c := doc.Lists[0].Counts
		if c == nil {
			return "none"
		}
		return strconv.Itoa(c.Movie) + " " + strconv.Itoa(c.Series)
	}
	if got := counts("/api/v1/me/watchlists"); got != "none" {
		t.Errorf("counts unasked: %s", got)
	}
	if got := counts("/api/v1/me/watchlists?include=counts"); got != "2 1" {
		t.Errorf("counts asked: %s, want 2 1", got)
	}
}

// Every id with a kind is in exactly one row, an episode or an unknown kind
// among the shows, in the order given; an id without one is in none.
func TestRowsOfAList(t *testing.T) {
	ids := []string{"m1", "s1", "gone", "e1", "a1", "m2"}
	kinds := map[string]string{"m1": "movie", "s1": "series", "e1": "episode", "a1": "album", "m2": "movie"}
	if got := strings.Join(inRow(ids, kinds, rowMovie), " "); got != "m1 m2" {
		t.Errorf("movies: %q", got)
	}
	if got := strings.Join(inRow(ids, kinds, rowSeries), " "); got != "s1 e1 a1" {
		t.Errorf("shows: %q", got)
	}
	if c := countsOf(ids, kinds); c.Movie != 2 || c.Series != 3 {
		t.Errorf("counts: %+v", c)
	}
}
