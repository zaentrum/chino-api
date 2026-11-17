package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// itemsOfList is the items of a GET /me/watchlists/{listId} answer, in order.
func itemsOfList(t *testing.T, body []byte) string {
	t.Helper()
	var l struct {
		Items []string `json:"items"`
	}
	if err := json.Unmarshal(body, &l); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	return strings.Join(l.Items, " ")
}

// countsOfLists is each list of a GET /me/watchlists answer: its name and
// itemCount, and its counts where it has them.
func countsOfLists(t *testing.T, body []byte) string {
	t.Helper()
	var v struct {
		Lists []struct {
			Name      string                     `json:"name"`
			ItemCount int                        `json:"itemCount"`
			Counts    map[string]json.RawMessage `json:"counts"`
		} `json:"lists"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	var out []string
	for _, l := range v.Lists {
		s := fmt.Sprintf("%s %d", l.Name, l.ItemCount)
		if l.Counts != nil {
			s += fmt.Sprintf(" movie %s series %s", l.Counts["movie"], l.Counts["series"])
		}
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

// Against Postgres (CHINO_API_TEST_DATABASE_URL): a list comes by kind, its
// movies with kind=movie and everything else with kind=series (a series, an
// episode, an id of no title), each newest first; the overview with
// include=counts has each list's counts by kind, which add up to its count;
// a capped viewer's leave out what its cap does not allow. A kind that is
// neither is 400. The kinds are katalog-api's, asked once and kept.
func Test8C18AListComesByKind(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	katalog := ratedKatalog(t)
	h := filesRouter(t, is, katalog.URL, st)
	ctx := context.Background()
	lists := map[string]string{}
	for _, user := range []string{"adult-2", "kid-2"} {
		def, err := st.EnsureDefaultList(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		// Added one after another: the one added last comes first.
		for _, id := range []string{"kid", "show6", "ep6", "gone", "adult"} {
			if err := st.AddWatchlistItem(ctx, user, def, id); err != nil {
				t.Fatal(err)
			}
		}
		other, err := st.CreateWatchlist(ctx, user, "Rainy days")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.AddWatchlistItem(ctx, user, other.ID, "adult"); err != nil {
			t.Fatal(err)
		}
		lists[user] = def
	}

	for _, tc := range []struct {
		user  string
		claim any
		query string
		code  int
		want  string
	}{
		{"adult-2", nil, "", http.StatusOK, "adult gone ep6 show6 kid"},
		{"adult-2", nil, "?kind=", http.StatusOK, "adult gone ep6 show6 kid"},
		{"adult-2", nil, "?kind=movie", http.StatusOK, "adult kid"},
		{"adult-2", nil, "?kind=series", http.StatusOK, "gone ep6 show6"},
		{"adult-2", nil, "?kind=film", http.StatusBadRequest, "kind is movie or series\n"},
		{"adult-2", nil, "?kind=Movie", http.StatusBadRequest, "kind is movie or series\n"},
		{"adult-2", nil, "?kind=episode", http.StatusBadRequest, "kind is movie or series\n"},
		{"kid-2", 12, "?kind=movie", http.StatusOK, "kid"},
		{"kid-2", 12, "?kind=series", http.StatusOK, "ep6 show6"},
	} {
		w := do(h, http.MethodGet, "/api/v1/me/watchlists/"+lists[tc.user]+tc.query, bearerWith(t, is, tc.user, tc.claim))
		got := w.Body.String()
		if w.Code == http.StatusOK {
			got = itemsOfList(t, w.Body.Bytes())
		}
		if w.Code != tc.code || got != tc.want {
			t.Errorf("%s, list%s: %d %q, want %d %q", tc.user, tc.query, w.Code, got, tc.code, tc.want)
		}
	}
	if w := do(h, http.MethodGet, "/api/v1/me/watchlists/wl_none?kind=movie", bearerWith(t, is, "adult-2", nil)); w.Code != http.StatusNotFound {
		t.Errorf("a list that is not the viewer's, by kind: %d", w.Code)
	}

	for _, tc := range []struct {
		user  string
		claim any
		query string
		want  string
	}{
		{"adult-2", nil, "", "Watchlist 5; Rainy days 1"},
		{"adult-2", nil, "?include=counts", "Watchlist 5 movie 2 series 3; Rainy days 1 movie 1 series 0"},
		{"adult-2", nil, "?include=extras,counts", "Watchlist 5 movie 2 series 3; Rainy days 1 movie 1 series 0"},
		{"adult-2", nil, "?include=extras", "Watchlist 5; Rainy days 1"},
		{"kid-2", 12, "", "Watchlist 3; Rainy days 0"},
		{"kid-2", 12, "?include=counts", "Watchlist 3 movie 1 series 2; Rainy days 0 movie 0 series 0"},
	} {
		w := do(h, http.MethodGet, "/api/v1/me/watchlists"+tc.query, bearerWith(t, is, tc.user, tc.claim))
		if got := countsOfLists(t, w.Body.Bytes()); w.Code != http.StatusOK || got != tc.want {
			t.Errorf("%s, the lists%s: %d %q, want %q", tc.user, tc.query, w.Code, got, tc.want)
		}
		if !strings.Contains(tc.query, "counts") && strings.Contains(w.Body.String(), `"counts"`) {
			t.Errorf("%s, the lists%s: counts without include=counts: %s", tc.user, tc.query, w.Body)
		}
	}

	// Asked once and kept: the same again asks katalog-api about no item.
	katalog.take()
	do(h, http.MethodGet, "/api/v1/me/watchlists?include=counts", bearerWith(t, is, "adult-2", nil))
	do(h, http.MethodGet, "/api/v1/me/watchlists/"+lists["adult-2"]+"?kind=series", bearerWith(t, is, "adult-2", nil))
	for _, r := range katalog.take() {
		if strings.Contains(r, "/api/v1/items/") {
			t.Errorf("kept, yet katalog-api was asked %s", r)
		}
	}

	// A katalog-api that fails: by kind there is no answer, whole as before.
	down := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "the catalog is down", http.StatusInternalServerError)
	})
	h = filesRouter(t, is, down.URL, st)
	adult := bearerWith(t, is, "adult-2", nil)
	for path, code := range map[string]int{
		"/api/v1/me/watchlists/" + lists["adult-2"] + "?kind=movie": http.StatusBadGateway,
		"/api/v1/me/watchlists?include=counts":                      http.StatusBadGateway,
		"/api/v1/me/watchlists/" + lists["adult-2"]:                 http.StatusOK,
		"/api/v1/me/watchlists":                                     http.StatusOK,
	} {
		if w := do(h, http.MethodGet, path, adult); w.Code != code || (code == http.StatusBadGateway && w.Body.String() != "catalog unavailable\n") {
			t.Errorf("a failing catalog, %s: %d %q, want %d", path, w.Code, w.Body, code)
		}
	}
}
