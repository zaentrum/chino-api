package http

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// GET /api/v1/search is katalog-api's search for the viewer: the titles in
// its order with their posters, how each matched and an episode's series with
// its poster, and the people with their portraits; the query, the types and
// the limits go on to katalog-api.
func TestSearchComesThroughTheRouter(t *testing.T) {
	kat := newFake(t, map[string]string{"/api/v1/search": `{"items":[
		{"id":"s1","type":"series","title":"Widow's Bay","match":"prefix"},
		{"id":"m1","type":"movie","title":"Black Widow","year":2021,"match":"words"},
		{"id":"e1","type":"episode","title":"The Widow's Walk","season_number":1,"episode_number":3,"parent_id":"s1",
		 "match":"words","series":{"id":"s1","title":"Widow's Bay","matched":true}}],
		"people":[{"id":"p1","name":"Ada Widow","credits":2,"has_profile":true,"match":"words"}]}`})
	h := router(t, kat.URL, "http://katalog-manager.invalid", false)

	w := do(h, "GET", "/api/v1/search?q=widow&types=movie,series&types=episode&limit=20&people=5", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("search: %d %s", w.Code, w.Body)
	}
	want := `{"items":[` +
		`{"id":"s1","type":"series","title":"Widow's Bay","poster_url":"/api/v1/items/s1/poster","backdrop_url":"/api/v1/items/s1/backdrop","match":"prefix"},` +
		`{"id":"m1","type":"movie","title":"Black Widow","year":2021,"poster_url":"/api/v1/items/m1/poster","backdrop_url":"/api/v1/items/m1/backdrop","match":"words"},` +
		`{"id":"e1","type":"episode","title":"The Widow's Walk","poster_url":"/api/v1/items/e1/poster",` +
		`"backdrop_url":"/api/v1/items/e1/backdrop","season_number":1,"episode_number":3,"parent_id":"s1","match":"words",` +
		`"series":{"id":"s1","title":"Widow's Bay","matched":true,"poster_url":"/api/v1/items/s1/poster"}}],` +
		`"people":[{"id":"p1","name":"Ada Widow","credits":2,"has_profile":true,"profile_url":"/api/v1/people/p1/profile","match":"words"}],` +
		`"product":"chino","source":"katalog"}` + "\n"
	if w.Body.String() != want {
		t.Errorf("search:\n got %s\nwant %s", w.Body, want)
	}
	reqs := kat.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests to katalog-api, want one", len(reqs))
	}
	if got := reqs[0].URL.Query(); got.Get("q") != "widow" || got.Get("types") != "movie,series,episode" ||
		got.Get("limit") != "20" || got.Get("people") != "5" {
		t.Errorf("katalog-api got %v", got)
	}

	// The defaults, and a blank query, which asks nothing.
	if w = do(h, "GET", "/api/v1/search?q=widow", nil); w.Code != http.StatusOK {
		t.Fatalf("search: %d", w.Code)
	}
	if got := kat.requests()[1].URL.Query(); got.Get("types") != "" || got.Get("limit") != "30" || got.Get("people") != "12" {
		t.Errorf("katalog-api got %v", got)
	}
	w = do(h, "GET", "/api/v1/search?q=%20", nil)
	if w.Code != http.StatusOK || w.Body.String() != `{"items":[],"people":[],"product":"chino","source":"katalog"}`+"\n" {
		t.Errorf("a blank query: %d %s", w.Code, w.Body)
	}
	if len(kat.requests()) != 2 {
		t.Error("a blank query asked katalog-api")
	}
}

// A katalog-api without the search is searched as the clients searched
// before: each type's list of the query, one after the other, and /people.
func TestSearchOfAKatalogAPIWithoutIt(t *testing.T) {
	kat := newFake(t, map[string]string{
		"/api/v1/movies": `{"items":[{"id":"m1","type":"movie","title":"Black Widow"}],"total":1}`,
		"/api/v1/series": `{"items":[{"id":"s1","type":"series","title":"Widow's Bay"}],"total":1}`,
		"/api/v1/people": `{"people":[{"id":"p1","name":"Ada Widow","credits":1,"has_profile":false}],"total":1}`,
	})
	h := router(t, kat.URL, "http://katalog-manager.invalid", false)
	w := do(h, "GET", "/api/v1/search?q=widow&types=movie,series", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("search: %d %s", w.Code, w.Body)
	}
	var body struct {
		Items []struct {
			ID    string `json:"id"`
			Match string `json:"match"`
		} `json:"items"`
		People []struct {
			Name string `json:"name"`
		} `json:"people"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 2 || body.Items[0].ID != "m1" || body.Items[1].ID != "s1" || body.Items[0].Match != "" ||
		len(body.People) != 1 || body.People[0].Name != "Ada Widow" {
		t.Errorf("search: %s", w.Body)
	}
	var paths []string
	for _, r := range kat.requests() {
		paths = append(paths, r.URL.Path+"?q="+r.URL.Query().Get("q"))
	}
	if strings.Join(paths, " ") != "/api/v1/search?q=widow /api/v1/movies?q=widow /api/v1/series?q=widow /api/v1/people?q=widow" {
		t.Errorf("katalog-api got %q", paths)
	}
}

// A catalog that fails is 502, as for the lists: not "nothing found".
func TestASearchTheCatalogFails(t *testing.T) {
	kat := newFake(t, map[string]string{"/api/v1/search": `not json`})
	h := router(t, kat.URL, "http://katalog-manager.invalid", false)
	if w := do(h, "GET", "/api/v1/search?q=widow", nil); w.Code != http.StatusBadGateway {
		t.Errorf("search: %d %s, want 502", w.Code, w.Body)
	}
}
