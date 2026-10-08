package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fileEpisode is an episode as katalog-api sends it, with the fields that say
// which file it plays.
type fileEpisode struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Title      string   `json:"title"`
	Season     int      `json:"season_number"`
	Episode    int      `json:"episode_number"`
	ParentID   string   `json:"parent_id"`
	CoveredBy  string   `json:"coveredBy,omitempty"`
	Covers     []string `json:"covers,omitempty"`
	EpisodeEnd int      `json:"episodeEnd,omitempty"`
}

// filed is the episodes of the series seriesID, one per code "SxxEyy" (id
// "sxxeyy"), in the order given; a code of a range, "S05E15-E16", is one file
// that holds those episodes: its holder, the first, with covers and
// episodeEnd, each other with coveredBy.
func filed(seriesID string, codes ...string) []fileEpisode {
	var out []fileEpisode
	for _, c := range codes {
		season, _ := strconv.Atoi(c[1:3])
		first, _ := strconv.Atoi(c[4:6])
		last := first
		if i := strings.Index(c, "-E"); i > 0 {
			last, _ = strconv.Atoi(c[i+2:])
		}
		holder := len(out)
		for n := first; n <= last; n++ {
			e := fileEpisode{ID: fmt.Sprintf("s%02de%02d", season, n), Type: "episode",
				Title: fmt.Sprintf("S%02dE%02d", season, n), Season: season, Episode: n, ParentID: seriesID}
			if n > first {
				e.CoveredBy = out[holder].ID
				out[holder].Covers = append(out[holder].Covers, e.ID)
				out[holder].EpisodeEnd = n
			}
			out = append(out, e)
		}
	}
	return out
}

// fileCatalog is a katalog-api of the series it is given, each by its codes
// as filed takes them: it answers a series' episodes, each episode and each
// series by id, and what a cap allows (everything). plain answers as a
// katalog-api that knows nothing of files that hold several episodes, down as
// one that does not answer. It counts the requests for each path.
type fileCatalog struct {
	*httptest.Server
	mu    sync.Mutex
	plain bool
	down  bool
	asked map[string]int
}

func newFileCatalog(t *testing.T, series map[string][]string) *fileCatalog {
	t.Helper()
	c := &fileCatalog{asked: map[string]int{}}
	lists := map[string][]fileEpisode{}
	items := map[string]any{}
	for id, codes := range series {
		lists[id] = filed(id, codes...)
		items[id] = map[string]string{"id": id, "type": "series", "title": "Series " + id}
		for _, e := range lists[id] {
			items[e.ID] = e
		}
	}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.asked[r.URL.Path]++
		plain, down := c.plain, c.down
		c.mu.Unlock()
		if down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		strip := func(e fileEpisode) fileEpisode {
			if plain {
				e.CoveredBy, e.Covers, e.EpisodeEnd = "", nil, 0
			}
			return e
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
		var body any
		switch {
		case r.URL.Path == "/api/v1/visible":
			body = map[string]any{"ids": strings.Split(r.URL.Query().Get("ids"), ",")}
		case len(parts) == 3 && parts[0] == "series" && parts[2] == "episodes" && lists[parts[1]] != nil:
			eps := []fileEpisode{}
			for _, e := range lists[parts[1]] {
				eps = append(eps, strip(e))
			}
			body = map[string]any{"items": eps}
		case len(parts) == 2 && parts[0] == "items" && items[parts[1]] != nil:
			body = items[parts[1]]
			if e, ok := body.(fileEpisode); ok {
				body = strip(e)
			}
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(c.Close)
	return c
}

// set makes the catalog answer as plain or down says.
func (c *fileCatalog) set(plain, down bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plain, c.down = plain, down
}

// requests is how often each path was asked for since the last call.
func (c *fileCatalog) requests() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.asked
	c.asked = map[string]int{}
	return out
}

// fileFieldsOf are the fields of an item as a client gets it that say which
// file it plays (coveredBy, covers, episodeEnd), those it has, as JSON.
func fileFieldsOf(t *testing.T, item json.RawMessage) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(item, &fields); err != nil {
		t.Fatalf("%s: %v", item, err)
	}
	var out []string
	for _, k := range []string{"coveredBy", "covers", "episodeEnd"} {
		if v, ok := fields[k]; ok {
			out = append(out, k+"="+string(v))
		}
	}
	return strings.Join(out, " ")
}

// An episode's detail and a series' episodes carry what katalog-api says of
// the file an episode plays, under its names: the holder's covers and
// episodeEnd, each covered episode's coveredBy. An episode with a file of its
// own has none of them, and neither has any episode of a katalog-api that
// knows nothing of it.
func TestTheEpisodesOfOneFileComeThroughToTheClients(t *testing.T) {
	kat := newFileCatalog(t, map[string][]string{"show": {"S05E14", "S05E15-E16", "S05E17"}})
	h := router(t, kat.URL, "http://katalog-manager.invalid", false)
	want := map[string]string{
		"s05e14": "",
		"s05e15": `covers=["s05e16"] episodeEnd=16`,
		"s05e16": `coveredBy="s05e15"`,
		"s05e17": "",
	}
	for _, plain := range []bool{false, true} {
		kat.set(plain, false)
		for id, fields := range want {
			if plain {
				fields = ""
			}
			w := do(h, "GET", "/api/v1/items/"+id, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", id, w.Code, w.Body)
			}
			if got := fileFieldsOf(t, w.Body.Bytes()); got != fields {
				t.Errorf("%s's detail (plain %v): %q, want %q", id, plain, got, fields)
			}
		}
		w := do(h, "GET", "/api/v1/series/show/episodes", nil)
		var list struct {
			Seasons []struct {
				Episodes []json.RawMessage `json:"episodes"`
			} `json:"seasons"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Seasons) != 1 {
			t.Fatalf("the series' episodes: %d %s", w.Code, w.Body)
		}
		var ids []string
		for _, e := range list.Seasons[0].Episodes {
			var it struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(e, &it)
			ids = append(ids, it.ID)
			fields := want[it.ID]
			if plain {
				fields = ""
			}
			if got := fileFieldsOf(t, e); got != fields {
				t.Errorf("%s in the series' episodes (plain %v): %q, want %q", it.ID, plain, got, fields)
			}
		}
		if strings.Join(ids, " ") != "s05e14 s05e15 s05e16 s05e17" {
			t.Errorf("the series' episodes: %v", ids)
		}
	}
}
