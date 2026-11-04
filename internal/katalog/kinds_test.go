package katalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// kindsCatalog answers the item route: "gone" names no title, "broken" fails.
func kindsCatalog(t *testing.T, asked *atomic.Int32, inFlight, most *atomic.Int32) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if n := inFlight.Add(1); n > most.Load() {
			most.Store(n)
		}
		defer inFlight.Add(-1)
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/items/")
		switch {
		case id == "gone":
			http.NotFound(w, r)
		case id == "broken":
			http.Error(w, "no", http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"` + id + `","type":"` + strings.SplitN(id, "-", 2)[0] + `","title":"t"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

func TestKindsAnswersEachTitleOnceAndLeavesOutAnIdOfNoTitle(t *testing.T) {
	var asked, inFlight, most atomic.Int32
	c := kindsCatalog(t, &asked, &inFlight, &most)
	got, err := c.Kinds(context.Background(), "b", []string{"movie-1", "series-1", "episode-1", "movie-1", "gone"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"movie-1": "movie", "series-1": "series", "episode-1": "episode"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for id, kind := range want {
		if got[id] != kind {
			t.Errorf("%s: got %q, want %q", id, got[id], kind)
		}
	}
	if n := asked.Load(); n != 4 {
		t.Errorf("asked %d times, want 4: a repeated id is asked once", n)
	}
}

func TestKindsKeepsWhatItLearnedButNotAnIdOfNoTitle(t *testing.T) {
	var asked, inFlight, most atomic.Int32
	c := kindsCatalog(t, &asked, &inFlight, &most)
	ids := []string{"movie-1", "gone"}
	for range 2 {
		if _, err := c.Kinds(context.Background(), "b", ids); err != nil {
			t.Fatal(err)
		}
	}
	// movie-1 once; gone both times, as a title may yet arrive under it.
	if n := asked.Load(); n != 3 {
		t.Errorf("asked %d times, want 3", n)
	}
}

func TestKindsFailsWholeWhenATitleGoesUnanswered(t *testing.T) {
	var asked, inFlight, most atomic.Int32
	c := kindsCatalog(t, &asked, &inFlight, &most)
	if got, err := c.Kinds(context.Background(), "b", []string{"movie-1", "broken"}); err == nil {
		t.Fatalf("no error, got %v", got)
	}
}

func TestKindsAsksABoundedNumberAtATime(t *testing.T) {
	var asked, inFlight, most atomic.Int32
	c := kindsCatalog(t, &asked, &inFlight, &most)
	ids := make([]string, 60)
	for i := range ids {
		ids[i] = "movie-" + strings.Repeat("x", i+1)
	}
	if _, err := c.Kinds(context.Background(), "b", ids); err != nil {
		t.Fatal(err)
	}
	if n := most.Load(); n > kindLookups {
		t.Errorf("%d at a time, want at most %d", n, kindLookups)
	}
	if n := asked.Load(); n != 60 {
		t.Errorf("asked %d, want 60", n)
	}
}
