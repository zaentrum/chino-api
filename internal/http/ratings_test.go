package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
	"github.com/zaentrum/chino-api/internal/store"
)

// claimCases are bearers' max_rating claims and the cap each holds its viewer
// to: the age of a whole number of years, none without the claim, and the
// strictest, 0, for any other value.
var claimCases = []struct {
	name  string
	claim any // nil: no claim at all
	want  string
}{
	{"no claim", nil, "uncapped"},
	{"12", 12, "12"},
	{"0", 0, "0"},
	{"18", 18, "18"},
	{"16.0", 16.0, "16"},
	{"12.5", 12.5, "0"},
	{"-1", -1, "0"},
	{`"12"`, "12", "0"},
	{"null", json.RawMessage("null"), "0"},
	{"true", true, "0"},
	{"[12]", []int{12}, "0"},
	{"an object", map[string]int{"age": 12}, "0"},
	{"1e300", 1e300, "0"},
}

// bearerWith is a bearer of is for sub with audience chino, whose
// max_rating claim is claim (none when nil).
func bearerWith(t *testing.T, is *issuer, sub string, claim any) http.Header {
	t.Helper()
	var extra map[string]any
	if claim != nil {
		extra = map[string]any{auth.MaxRatingClaim: claim}
	}
	return http.Header{"Authorization": {"Bearer " + is.tokenWith(t, sub, "chino", extra)}}
}

// The stream token a viewer mints carries the cap of its bearer's max_rating:
// none without the claim, the age of one, the strictest cap for a claim
// that is no whole number of years, said once.
func TestTheStreamTokenCarriesTheBearersCap(t *testing.T) {
	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	is := newIssuer(t)
	h := oidcRouter(t, is)
	signer, err := auth.NewSigner(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	malformed := 0
	for _, tc := range claimCases {
		w := do(h, "POST", "/api/v1/me/stream-token", bearerWith(t, is, "kid-1", tc.claim))
		var body struct {
			Token string `json:"stream_token"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body)
		}
		user, maxRating, err := signer.VerifyCapped(body.Token)
		got := "uncapped"
		if maxRating != nil {
			got = strconv.Itoa(*maxRating)
		}
		if err != nil || user != "kid-1" || got != tc.want {
			t.Errorf("a bearer with %s: the token's user %q cap %s (%v), want kid-1 at %s", tc.name, user, got, err, tc.want)
		}
		if tc.want == "0" && tc.name != "0" {
			malformed++
		}
	}
	if n := strings.Count(logged.String(), "no whole number of years"); malformed > 0 && n != 1 {
		t.Errorf("malformed claims said %d times, want once:\n%s", n, logged.String())
	}
}

// ratedAges are the titles of the rated catalog, by the age a viewer must be
// (-1: unrated), and the title of each sidecar subtitle.
var (
	ratedAges     = map[string]int{"kid": 6, "adult": 16, "unrated": -1, "show6": 6, "ep6": 6, "show16": 16, "ep16": 16}
	ratedSubtitle = map[string]string{"sub-kid": "kid", "sub-adult": "adult"}
)

// allowedAt is whether a viewer at the cap of r's max_rating parameter may be
// served id; every viewer without one may.
func allowedAt(r *http.Request, id string) bool {
	v, ok := r.URL.Query()["max_rating"]
	if !ok {
		_, there := ratedAges[id]
		return there
	}
	maxAge, err := strconv.Atoi(v[0])
	age, there := ratedAges[id]
	return err == nil && there && age >= 0 && age <= maxAge
}

// recorder is an upstream that records the requests it gets and answers
// with answer.
type recorder struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []string
}

func newRecorder(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) *recorder {
	t.Helper()
	rec := &recorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, r.Method+" "+r.URL.String())
		rec.mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(rec.Close)
	return rec
}

// take is what the upstream got since the last take.
func (rec *recorder) take() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := rec.reqs
	rec.reqs = nil
	return out
}

// ratedKatalog is a katalog-api for the rated catalog that holds a viewer to
// the cap chino-api passes, as katalog-api does: a title the cap does not
// allow is 404 by id, and left out of a list.
func ratedKatalog(t *testing.T) *recorder {
	return newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		item := func(id string) string {
			typ := map[bool]string{true: "series", false: "movie"}[strings.HasPrefix(id, "show")]
			if strings.HasPrefix(id, "ep") {
				typ = "episode"
			}
			return `{"id":"` + id + `","type":"` + typ + `","title":"` + id + `","season_number":1,"episode_number":1}`
		}
		list := func(ids ...string) string {
			var out []string
			for _, id := range ids {
				if allowedAt(r, id) {
					out = append(out, item(id))
				}
			}
			return `{"items":[` + strings.Join(out, ",") + `],"total":` + strconv.Itoa(len(out)) + `}`
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/visible":
			if _, ok := r.URL.Query()["max_rating"]; !ok {
				http.Error(w, `{"error":"max_rating is required"}`, http.StatusBadRequest)
				return
			}
			var out []string
			for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
				if allowedAt(r, id) {
					out = append(out, `"`+id+`"`)
				}
			}
			_, _ = io.WriteString(w, `{"ids":[`+strings.Join(out, ",")+`]}`)
		case len(parts) == 3 && parts[0] == "subtitles" && parts[2] == "asset":
			if it, ok := ratedSubtitle[parts[1]]; ok {
				_, _ = io.WriteString(w, `{"itemId":"`+it+`","path":"/subs/`+parts[1]+`.vtt"}`)
				return
			}
			http.NotFound(w, r)
		case len(parts) >= 2 && (parts[0] == "items" || parts[0] == "series") && !allowedAt(r, parts[1]):
			http.Error(w, "not found", http.StatusNotFound)
		case len(parts) == 3 && parts[0] == "series" && parts[2] == "episodes":
			_, _ = io.WriteString(w, list("ep"+strings.TrimPrefix(parts[1], "show")))
		case len(parts) == 2 && parts[0] == "items":
			_, _ = io.WriteString(w, item(parts[1]))
		case len(parts) == 3 && parts[0] == "items":
			_, _ = io.WriteString(w, `{"items":[]}`)
		case len(parts) == 1:
			_, _ = io.WriteString(w, list("kid", "adult", "unrated", "show6", "show16", "ep6", "ep16"))
		default:
			http.NotFound(w, r)
		}
	})
}

// ratedRouter is chino-api with OIDC against is, the rated katalog-api, and
// chino-stream and katalog-manager that serve anything.
func ratedRouter(t *testing.T, is *issuer) (h http.Handler, katalog, stream, manager *recorder) {
	t.Helper()
	katalog = ratedKatalog(t)
	media := func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "media bytes") }
	stream, manager = newRecorder(t, media), newRecorder(t, media)
	h, err := NewRouter(config.Config{
		OIDCIssuer:        is.URL,
		OIDCAudience:      "chino",
		OIDCEnabled:       true,
		KatalogBaseURL:    katalog.URL,
		ArtworkBaseURL:    manager.URL,
		KatalogManagerURL: manager.URL,
		StreamBaseURL:     stream.URL,
		StreamSigningKey:  signingKey,
	}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	return h, katalog, stream, manager
}

// titleRoutes are the routes of one title chino-api serves, as the router
// holds them: every route a title id names ({id}, {itemId}), and a sidecar
// subtitle's; a person's routes ({id} names a person) are none of them.
func titleRoutes(t *testing.T, h http.Handler) [][2]string {
	t.Helper()
	var out [][2]string
	err := chi.Walk(h.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if (strings.Contains(route, "{id}") || strings.Contains(route, "{itemId}")) && !strings.HasPrefix(route, "/api/v1/people/") {
			out = append(out, [2]string{method, route})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var routeParam = regexp.MustCompile(`\{(\w+)(:[^}]*)?\}`)

// pathOf is route with title as the title (or subtitle) it names and every
// other parameter one the route takes.
func pathOf(route, title string) string {
	return routeParam.ReplaceAllStringFunc(route, func(p string) string {
		m := routeParam.FindStringSubmatch(p)
		switch m[1] {
		case "id", "itemId":
			if strings.HasPrefix(route, "/api/v1/play/subs/") {
				return "sub-" + title
			}
			return title
		case "rendId":
			if strings.HasPrefix(m[2], ":s") {
				return "s0"
			}
			return "v0"
		case "seg":
			return "00001"
		case "quality":
			return "high"
		case "listId":
			return "l1"
		case "extraId":
			return testExtra
		}
		return "1" // audioIdx, n, streamIndex
	})
}

// Every route of one title answers a capped viewer 404 for a title its cap
// does not allow, an unrated one and an id that names none, the same 404
// whatever the route, before anything goes upstream but the question of what
// the cap allows: with its bearer, and on the routes that take one with its
// stream token. A title the cap allows goes through as before, and so does
// every title for a viewer without a cap, whom nobody asks about.
func TestEveryRouteOfOneTitleIs404ForATitleAboveTheCap(t *testing.T) {
	is := newIssuer(t)
	h, katalog, stream, manager := ratedRouter(t, is)
	signer, err := auth.NewSigner(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	streamTok, _ := signer.MintCapped("kid-1", 12, time.Hour)
	capped := bearerWith(t, is, "kid-1", 12)
	malformed := bearerWith(t, is, "kid-1", "twelve")
	adult := bearerWith(t, is, "adult-1", nil)

	routes := titleRoutes(t, h)
	if len(routes) < 40 {
		t.Fatalf("%d routes of one title, want every one of them (40 and more)", len(routes))
	}
	upstream := func() []string { return append(append(stream.take(), manager.take()...), katalog.take()...) }
	asked := func(reqs []string) (others []string) {
		for _, r := range reqs {
			if !strings.Contains(r, "/api/v1/visible?") && !strings.Contains(r, "/api/v1/subtitles/") {
				others = append(others, r)
			}
		}
		return others
	}
	streamRoutes := 0
	for _, rt := range routes {
		method, route := rt[0], rt[1]
		for _, title := range []string{"adult", "unrated", "no-such-title"} {
			path := pathOf(route, title)
			creds := []struct {
				name   string
				query  string
				header http.Header
			}{{"a bearer capped at 12", "", capped}, {"a bearer whose cap is no number", "", malformed}}
			if w := do(h, method, path+"?stream="+streamTok, nil); w.Code != http.StatusUnauthorized {
				creds = append(creds, struct {
					name   string
					query  string
					header http.Header
				}{"a stream token capped at 12", "?stream=" + streamTok, nil})
				if title == "adult" {
					streamRoutes++
				}
			}
			upstream()
			for _, c := range creds {
				w := do(h, method, path+c.query, c.header)
				if w.Code != http.StatusNotFound || w.Body.String() != "not found\n" {
					t.Errorf("%s %s, %s: %d %q, want 404 not found", method, path, c.name, w.Code, w.Body)
				}
				if got := asked(upstream()); len(got) != 0 {
					t.Errorf("%s %s, %s: went upstream %q", method, path, c.name, got)
				}
			}
		}

		// The title the cap allows goes through, and every title for the uncapped.
		for _, tc := range []struct {
			title  string
			header http.Header
		}{{"kid", capped}, {"adult", adult}, {"kid", adult}} {
			path := pathOf(route, tc.title)
			w := do(h, method, path, tc.header)
			reqs := upstream()
			if w.Code == http.StatusNotFound && w.Body.String() == "not found\n" && len(asked(reqs)) == 0 {
				t.Errorf("%s %s: held back (%d %q)", method, path, w.Code, w.Body)
			}
			if tc.header["Authorization"][0] == adult["Authorization"][0] && len(reqs) != len(asked(reqs)) {
				t.Errorf("%s %s: an uncapped viewer's title was asked about: %q", method, path, reqs)
			}
		}
	}
	if streamRoutes < 20 {
		t.Errorf("%d routes of one title took the stream token, want the media routes (20 and more)", streamRoutes)
	}
}

// katalog-api failing to say what a cap allows serves a capped viewer
// nothing: 502, and nothing upstream.
func TestACapWithoutAnAnswerServesNothing(t *testing.T) {
	is := newIssuer(t)
	stream := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "media") })
	h, err := NewRouter(config.Config{OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: "http://127.0.0.1:1", StreamBaseURL: stream.URL, ArtworkBaseURL: stream.URL,
		StreamSigningKey: signingKey}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/items/kid/play/master.m3u8", "/api/v1/items/kid/poster", "/api/v1/play/subs/sub-kid.vtt"} {
		if w := do(h, "GET", path, bearerWith(t, is, "kid-1", 12)); w.Code != http.StatusBadGateway {
			t.Errorf("%s with katalog-api down: %d %q, want 502", path, w.Code, w.Body)
		}
	}
	if got := stream.take(); len(got) != 0 {
		t.Errorf("went upstream: %q", got)
	}
}

// What katalog-api does not find (an id of no title, or one the viewer's cap
// leaves out) is 404 on the routes that list a title's parts too: a series'
// episodes, its next episode, an item's segments. A series without episodes
// is still an empty one.
func TestWhatKatalogDoesNotFindIs404(t *testing.T) {
	kat := newFake(t, map[string]string{"/api/v1/series/empty/episodes": `{"items":[]}`,
		"/api/v1/items/bare/segments": `{"items":[]}`})
	h := router(t, kat.URL, "http://katalog-manager.invalid", false)
	for path, want := range map[string]int{
		"/api/v1/series/gone/episodes":      http.StatusNotFound,
		"/api/v1/series/gone/next-episode":  http.StatusNotFound,
		"/api/v1/items/gone/segments":       http.StatusNotFound,
		"/api/v1/series/empty/episodes":     http.StatusOK,
		"/api/v1/series/empty/next-episode": http.StatusOK,
		"/api/v1/items/bare/segments":       http.StatusOK,
	} {
		if w := do(h, "GET", path, nil); w.Code != want {
			t.Errorf("%s: %d %q, want %d", path, w.Code, w.Body, want)
		}
	}
}

// testDatabase names the PostgreSQL the tests of chino-api's own lists use;
// they are skipped without it. Each test works in a schema of its own.
const testDatabase = "CHINO_API_TEST_DATABASE_URL"

// testStore is chino-api's store, migrated, in a schema of its own on the
// database testDatabase names, dropped when the test ends.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv(testDatabase)
	if dsn == "" {
		t.Skipf("set %s to run the tests of the lists chino-api keeps", testDatabase)
	}
	ctx := context.Background()
	schema := fmt.Sprintf("chino_api_test_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	st, err := store.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

// The lists a viewer keeps (watchlists and their counts, the default
// watchlist, likes, memberships), its history and what it goes on watching
// leave out what its cap does not allow, and every title of a viewer without
// a cap is in them as before.
func TestAViewersListsAreHeldToItsCap(t *testing.T) {
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
	titles := []string{"adult", "kid", "unrated", "ep16", "ep6"}
	for _, user := range []string{"kid-1", "adult-1"} {
		def, err := st.EnsureDefaultList(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		other, err := st.CreateWatchlist(ctx, user, "Saved for later")
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range titles {
			if err := st.AddWatchlistItem(ctx, user, def, id); err != nil {
				t.Fatal(err)
			}
			if err := st.SetFlag(ctx, store.LikesTable, user, id, true); err != nil {
				t.Fatal(err)
			}
			if err := st.SaveProgress(ctx, user, id, 120, 3600); err != nil {
				t.Fatal(err)
			}
			// History is another viewer's: what a viewer watched is finished,
			// and goes from what it goes on watching.
			if err := st.MarkWatched(ctx, user+"-history", id); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.AddWatchlistItem(ctx, user, other.ID, "adult"); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(t *testing.T, body string, key string) string {
		t.Helper()
		var v map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		var list []json.RawMessage
		_ = json.Unmarshal(v[key], &list)
		var out []string
		for _, e := range list {
			var s string
			if json.Unmarshal(e, &s) == nil {
				out = append(out, s)
				continue
			}
			var it struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(e, &it)
			out = append(out, it.ID)
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}
	for _, tc := range []struct {
		user   string
		claim  any
		titles string
		counts string
	}{
		{"kid-1", 12, "ep6 kid", "Watchlist 2, Saved for later 0"},
		{"kid-1", 5, "", "Watchlist 0, Saved for later 0"},
		{"adult-1", nil, "adult ep16 ep6 kid unrated", "Watchlist 5, Saved for later 1"},
	} {
		bearer := bearerWith(t, is, tc.user, tc.claim)
		def, _ := st.EnsureDefaultList(ctx, tc.user)
		for path, header := range map[string]http.Header{"/api/v1/me/watchlist": bearer, "/api/v1/me/watchlists/" + def: bearer,
			"/api/v1/me/likes": bearer, "/api/v1/me/continue-watching": bearer,
			"/api/v1/me/watched": bearerWith(t, is, tc.user+"-history", tc.claim)} {
			w := do(h, "GET", path, header)
			if got := ids(t, w.Body.String(), "items"); w.Code != http.StatusOK || got != tc.titles {
				t.Errorf("%s capped at %v, %s: %d %q, want %q", tc.user, tc.claim, path, w.Code, got, tc.titles)
			}
		}
		w := do(h, "GET", "/api/v1/me/watchlists/memberships?ids="+strings.Join(titles, ","), bearer)
		var m struct {
			Memberships map[string][]string `json:"memberships"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		var in []string
		for id := range m.Memberships {
			in = append(in, id)
		}
		sort.Strings(in)
		if strings.Join(in, " ") != tc.titles {
			t.Errorf("%s capped at %v, memberships: %v, want %q", tc.user, tc.claim, m.Memberships, tc.titles)
		}
		w = do(h, "GET", "/api/v1/me/watchlists", bearer)
		var lists struct {
			Lists []struct {
				Name      string `json:"name"`
				ItemCount int    `json:"itemCount"`
			} `json:"lists"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &lists)
		var counts []string
		for _, l := range lists.Lists {
			counts = append(counts, l.Name+" "+strconv.Itoa(l.ItemCount))
		}
		if got := strings.Join(counts, ", "); got != tc.counts {
			t.Errorf("%s capped at %v, the lists: %q, want %q", tc.user, tc.claim, got, tc.counts)
		}
	}
}

// The Zap feed and the packaged ids are chino-stream's, and know no rating:
// a capped viewer's leave out the titles its cap does not allow, keeping
// everything else of the answer; a viewer without a cap gets them as
// chino-stream sends them.
func TestTheZapFeedsAreHeldToTheCap(t *testing.T) {
	is := newIssuer(t)
	const feed = `{"items":[{"id":"adult","title":"Adult","seek_sec":12.5},{"id":"kid","title":"Kid","seek_sec":3.0},` +
		`{"id":"unrated","title":"Unrated"},{"id":"ep6","title":"Six","seek_sec":1.0}],"pool_size":8,"warmed":4}`
	const packaged = `{"ids":["adult","kid","unrated","ep16","ep6"]}`
	stream := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, max-age=30")
		switch r.URL.Path {
		case "/api/play/zap-feed":
			_, _ = io.WriteString(w, feed)
		case "/api/play/packaged-ids":
			_, _ = io.WriteString(w, packaged)
		default:
			http.NotFound(w, r)
		}
	})
	katalog := ratedKatalog(t)
	h, err := NewRouter(config.Config{OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: katalog.URL, StreamBaseURL: stream.URL, ArtworkBaseURL: "http://katalog-manager.invalid",
		StreamSigningKey: signingKey}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := auth.NewSigner(signingKey)
	tok, _ := signer.MintCapped("kid-1", 12, time.Hour)
	for _, tc := range []struct {
		name          string
		query         string
		header        http.Header
		feed, packIDs string
	}{
		{"a capped bearer", "?limit=4", bearerWith(t, is, "kid-1", 12),
			`{"items":[{"id":"kid","title":"Kid","seek_sec":3.0},{"id":"ep6","title":"Six","seek_sec":1.0}],"pool_size":8,"warmed":4}`,
			`{"ids":["kid","ep6"]}`},
		{"a capped stream token", "?limit=4&stream=" + tok, nil,
			`{"items":[{"id":"kid","title":"Kid","seek_sec":3.0},{"id":"ep6","title":"Six","seek_sec":1.0}],"pool_size":8,"warmed":4}`,
			`{"ids":["kid","ep6"]}`},
		{"a viewer without a cap", "?limit=4", bearerWith(t, is, "adult-1", nil), feed, packaged},
	} {
		w := do(h, "GET", "/api/v1/play/zap-feed"+tc.query, tc.header)
		var got, want any
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		_ = json.Unmarshal([]byte(tc.feed), &want)
		if w.Code != http.StatusOK || !reflect.DeepEqual(got, want) || w.Header().Get("Cache-Control") != "private, max-age=30" {
			t.Errorf("%s, the Zap feed: %d %s, want %s", tc.name, w.Code, w.Body, tc.feed)
		}
		if r := stream.take(); len(r) != 1 || !strings.Contains(r[0], "limit=4") {
			t.Errorf("%s: chino-stream got %q, want the query passed on", tc.name, r)
		}
		w = do(h, "GET", "/api/v1/play/packaged-ids"+tc.query, tc.header)
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != tc.packIDs {
			t.Errorf("%s, the packaged ids: %d %s, want %s", tc.name, w.Code, w.Body, tc.packIDs)
		}
		stream.take()
	}
}
