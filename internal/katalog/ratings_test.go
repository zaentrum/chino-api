package katalog

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/chino-api/internal/auth"
)

// catalogCalls is every call the client makes to katalog-api for a viewer,
// each by the path it asks.
var catalogCalls = map[string]func(c *Client, ctx context.Context) error{
	"/api/v1/movies": func(c *Client, ctx context.Context) error {
		_, err := c.ListMoviesFiltered(ctx, "tok", "", 10, 0, nil)
		return err
	},
	"/api/v1/series": func(c *Client, ctx context.Context) error {
		_, err := c.ListSeriesFiltered(ctx, "tok", "", 10, 0, nil)
		return err
	},
	"/api/v1/episodes": func(c *Client, ctx context.Context) error {
		_, err := c.ListEpisodesFiltered(ctx, "tok", "", 10, 0, nil)
		return err
	},
	"/api/v1/albums": func(c *Client, ctx context.Context) error { _, err := c.ListAlbums(ctx, "tok", "", 10); return err },
	"/api/v1/items":  func(c *Client, ctx context.Context) error { _, err := c.ListAll(ctx, "tok", "q", 10); return err },
	"/api/v1/items/m1": func(c *Client, ctx context.Context) error {
		_, err := c.GetItemDetail(ctx, "tok", "m1")
		return err
	},
	"/api/v1/items/m1/similar": func(c *Client, ctx context.Context) error {
		_, err := c.ListSimilar(ctx, "tok", "m1", 5)
		return err
	},
	"/api/v1/items/m1/segments": func(c *Client, ctx context.Context) error {
		_, err := c.ListSegments(ctx, "tok", "m1")
		return err
	},
	"/api/v1/series/s1/episodes": func(c *Client, ctx context.Context) error {
		_, err := c.ListSeriesEpisodes(ctx, "tok", "s1")
		return err
	},
	"/api/v1/genres": func(c *Client, ctx context.Context) error { _, err := c.ListGenres(ctx, "tok"); return err },
	"/api/v1/people": func(c *Client, ctx context.Context) error {
		_, err := c.SearchPeople(ctx, "tok", "ada", 5)
		return err
	},
	"/api/v1/people/p1": func(c *Client, ctx context.Context) error {
		_, err := c.GetPerson(ctx, "tok", "p1", 5, "de", "")
		return err
	},
}

// Every catalog request carries the viewer's rating cap as max_rating, with
// the rest of its query; an uncapped viewer's carries none, as before.
func TestEveryCatalogRequestCarriesTheCap(t *testing.T) {
	routes := map[string]string{}
	for path := range catalogCalls {
		routes[path] = `{"id":"x","items":[],"people":[],"genres":[]}`
	}
	up := newUpstream(t, routes)
	c := New(up.URL)
	for path, call := range catalogCalls {
		for _, tc := range []struct {
			ctx  context.Context
			want string
		}{
			{context.Background(), ""},
			{auth.WithMaxRating(context.Background(), 12), "12"},
			{auth.WithMaxRating(context.Background(), 0), "0"},
		} {
			if err := call(c, tc.ctx); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			r := up.last(t)
			q := r.URL.Query()
			if r.URL.Path != path || q.Get("max_rating") != tc.want || len(q["max_rating"]) > 1 ||
				r.Header.Get("Authorization") != "Bearer tok" {
				t.Errorf("%s for a viewer capped at %q went as %s (%s)", path, tc.want, r.URL, r.Header.Get("Authorization"))
			}
			if _, ok := q["max_rating"]; ok != (tc.want != "") {
				t.Errorf("%s: max_rating sent %v, want %v", path, ok, tc.want != "")
			}
		}
	}
	// The rest of a query stays as it was.
	_ = catalogCalls["/api/v1/items/m1"](c, auth.WithMaxRating(context.Background(), 6))
	if q := up.last(t).URL.Query(); q.Get("include") != "genres,cast,subtitles,trailers,segments" {
		t.Errorf("an item's include: %q", q.Get("include"))
	}
	_ = catalogCalls["/api/v1/people/p1"](c, auth.WithMaxRating(context.Background(), 6))
	if q := up.last(t).URL.Query(); q.Get("lang") != "de" || q.Get("limit") != "5" {
		t.Errorf("a person's query: %v", q)
	}
}

// visibleCatalog is a katalog-api answering /api/v1/visible for the titles it
// rates (an age, -1 unrated, hidden from the capped), counting how often
// each id was asked about.
type visibleCatalog struct {
	*httptest.Server
	mu    sync.Mutex
	asked map[string]int
	fail  bool
}

func newVisibleCatalog(t *testing.T, ages map[string]int) *visibleCatalog {
	t.Helper()
	v := &visibleCatalog{asked: map[string]int{}}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		defer v.mu.Unlock()
		if v.fail {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		switch {
		case r.URL.Path == "/api/v1/visible":
			var maxAge int
			if _, err := fmt.Sscan(r.URL.Query().Get("max_rating"), &maxAge); err != nil {
				http.Error(w, "max_rating is required", http.StatusBadRequest)
				return
			}
			var out []string
			for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
				v.asked[id]++
				if age, ok := ages[id]; ok && age >= 0 && age <= maxAge {
					out = append(out, `"`+id+`"`)
				}
			}
			_, _ = io.WriteString(w, `{"ids":[`+strings.Join(out, ",")+`]}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/subtitles/"):
			v.asked[r.URL.Path]++
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/subtitles/"), "/asset")
			if !strings.HasPrefix(id, "sub-") {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, `{"itemId":"`+strings.TrimPrefix(id, "sub-")+`","path":"/x.vtt"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(v.Close)
	return v
}

// Visible asks katalog-api which titles a cap allows, in batches, in the
// order given, each once, and keeps the answers a while: a title asked about
// again is not asked again, at that cap; another cap asks anew. A failure is
// an error, kept for nothing.
func TestVisibleAsksOnceInAWhile(t *testing.T) {
	ages := map[string]int{"m6": 6, "m12": 12, "m16": 16, "mu": -1}
	for i := 0; i < 450; i++ {
		ages[fmt.Sprintf("x%d", i)] = i % 20
	}
	v := newVisibleCatalog(t, ages)
	c := New(v.URL)
	ctx := context.Background()

	got, err := c.Visible(ctx, 12, []string{"m16", "m6", "nothing", "mu", "m12", "m6"})
	if err != nil || strings.Join(got, " ") != "m6 m12" {
		t.Fatalf("capped at 12: %q %v", got, err)
	}
	got, _ = c.Visible(ctx, 12, []string{"m12", "m16", "m6"})
	if strings.Join(got, " ") != "m12 m6" || v.asked["m12"] != 1 || v.asked["m16"] != 1 {
		t.Errorf("asked again: %q, asked %v", got, v.asked)
	}
	if ok, _ := c.VisibleOne(ctx, 16, "m16"); !ok || v.asked["m16"] != 2 {
		t.Errorf("another cap: %v, asked %d times", ok, v.asked["m16"])
	}

	var many, allowed []string
	for i := 0; i < 450; i++ {
		many = append(many, fmt.Sprintf("x%d", i))
		if i%20 <= 5 {
			allowed = append(allowed, fmt.Sprintf("x%d", i))
		}
	}
	got, err = c.Visible(ctx, 5, many)
	if err != nil || strings.Join(got, " ") != strings.Join(allowed, " ") {
		t.Errorf("450 ids, in three batches: %d of them, %v; want %d", len(got), err, len(allowed))
	}

	v.mu.Lock()
	v.fail = true
	v.mu.Unlock()
	if _, err := c.Visible(ctx, 12, []string{"m6", "another"}); err == nil {
		t.Error("katalog-api failing: no error")
	}
	if ok, err := c.VisibleOne(ctx, 12, "m6"); err != nil || !ok {
		t.Errorf("a title known within its time, katalog-api failing: %v %v", ok, err)
	}

	// Kept for visibleTTL only.
	c.visible.mu.Lock()
	for k, a := range c.visible.visible {
		a.until = time.Now().Add(-time.Second)
		c.visible.visible[k] = a
	}
	c.visible.mu.Unlock()
	if _, err := c.VisibleOne(ctx, 12, "m6"); err == nil {
		t.Error("an answer past its time was used")
	}
}

// SubtitleItem is the title a sidecar subtitle belongs to, "" for one there
// is not, each asked once.
func TestSubtitleItem(t *testing.T) {
	v := newVisibleCatalog(t, nil)
	c := New(v.URL)
	for i := 0; i < 2; i++ {
		if item, err := c.SubtitleItem(context.Background(), "sub-m16"); err != nil || item != "m16" {
			t.Fatalf("sub-m16: %q %v", item, err)
		}
		if item, err := c.SubtitleItem(context.Background(), "nothing"); err != nil || item != "" {
			t.Fatalf("a subtitle there is not: %q %v", item, err)
		}
	}
	if v.asked["/api/v1/subtitles/sub-m16/asset"] != 1 || v.asked["/api/v1/subtitles/nothing/asset"] != 1 {
		t.Errorf("asked %v, want each once", v.asked)
	}
}

// An item's rating comes through as katalog-api sends it: its age (0 too)
// and the certification it comes from, and nothing of a rating where
// katalog-api sends none.
func TestAnItemsRatingComesThrough(t *testing.T) {
	up := newUpstream(t, map[string]string{
		"/api/v1/items/m0":  `{"id":"m0","type":"movie","title":"For All","min_age":0,"certification":"0","certification_country":"DE"}`,
		"/api/v1/items/m13": `{"id":"m13","type":"movie","title":"Teen","min_age":13,"certification":"PG-13","certification_country":"US"}`,
		"/api/v1/items/mu":  `{"id":"mu","type":"movie","title":"Unrated"}`,
		"/api/v1/movies":    `{"items":[{"id":"m13","type":"movie","title":"Teen","min_age":13,"certification":"PG-13","certification_country":"US"}]}`,
	})
	c := New(up.URL)
	for id, want := range map[string]string{
		"m0":  `{"id":"m0","type":"movie","title":"For All","min_age":0,"certification":"0","certification_country":"DE","poster_url":"/api/v1/items/m0/poster","backdrop_url":"/api/v1/items/m0/backdrop"}`,
		"m13": `{"id":"m13","type":"movie","title":"Teen","min_age":13,"certification":"PG-13","certification_country":"US","poster_url":"/api/v1/items/m13/poster","backdrop_url":"/api/v1/items/m13/backdrop"}`,
		"mu":  `{"id":"mu","type":"movie","title":"Unrated","poster_url":"/api/v1/items/mu/poster","backdrop_url":"/api/v1/items/mu/backdrop"}`,
	} {
		it, err := c.GetItem(context.Background(), "tok", id)
		if err != nil || it == nil {
			t.Fatalf("%s: %v %v", id, it, err)
		}
		sameJSON(t, id, it, want)
	}
	list, err := c.ListMovies(context.Background(), "tok", "", 10)
	if err != nil || len(list) != 1 || list[0].MinAge == nil || *list[0].MinAge != 13 || list[0].Certification != "PG-13" {
		t.Errorf("a list's item: %+v %v", list, err)
	}
}
