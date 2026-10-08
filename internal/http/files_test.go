package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
	"github.com/zaentrum/chino-api/internal/katalog"
	"github.com/zaentrum/chino-api/internal/store"
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

// filesRouter is chino-api with OIDC against is, katalog-api at katalogURL
// and its own lists in st.
func filesRouter(t *testing.T, is *issuer, katalogURL string, st *store.Store) http.Handler {
	t.Helper()
	h, err := NewRouter(config.Config{OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: katalogURL, StreamBaseURL: "http://chino-stream.invalid",
		ArtworkBaseURL: "http://katalog-manager.invalid", StreamSigningKey: signingKey}, st, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// send is a request of method to path with header and the JSON body.
func send(h http.Handler, method, path string, header http.Header, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range header {
		r.Header[k] = v
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// positions is the position GET /items/{id}/progress reads for the viewer of
// header, "id=position" for each of ids.
func positions(t *testing.T, h http.Handler, header http.Header, ids ...string) string {
	t.Helper()
	var out []string
	for _, id := range ids {
		w := do(h, "GET", "/api/v1/items/"+id+"/progress", header)
		var body struct {
			PositionSec int `json:"position_sec"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("%s's progress: %d %s", id, w.Code, w.Body)
		}
		out = append(out, id+"="+strconv.Itoa(body.PositionSec))
	}
	return strings.Join(out, " ")
}

// watchedOf is which of ids userID has watched, in id order.
func watchedOf(t *testing.T, st *store.Store, userID string, ids ...string) string {
	t.Helper()
	got, err := st.WatchedAtBatch(context.Background(), userID, ids)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for id := range got {
		out = append(out, id)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// A viewer's progress and watched state are a file's: a write for any
// episode of a file that holds several - a covered episode, its holder -
// goes to each of them, the same position, the same watched or unwatched,
// and a write for an episode with a file of its own to it alone. Each read
// is the episode's own row. katalog-api is asked once in a while which
// episodes a file holds, not for each write; another viewer's rows are not
// touched, and a capped viewer's go to the file as an uncapped one's.
func TestAWriteForAnEpisodeOfAFileIsAWriteForEach(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	kat := newFileCatalog(t, map[string][]string{
		"show": {"S05E14", "S05E15-E16", "S05E17"},
		"saga": {"S06E01-E03", "S06E04"},
	})
	h := filesRouter(t, is, kat.URL, st)
	viewer, other := bearerWith(t, is, "viewer-1", nil), bearerWith(t, is, "viewer-2", nil)
	show := []string{"s05e14", "s05e15", "s05e16", "s05e17"}
	saga := []string{"s06e01", "s06e02", "s06e03", "s06e04"}

	for _, step := range []struct {
		id, body, want string
	}{
		// The covered episode: its holder too.
		{"s05e16", `{"position_sec":600,"duration_sec":2700}`, "s05e14=0 s05e15=600 s05e16=600 s05e17=0"},
		// The holder: the episode it covers too.
		{"s05e15", `{"position_sec":900,"duration_sec":2700}`, "s05e14=0 s05e15=900 s05e16=900 s05e17=0"},
		// An episode with a file of its own: it alone.
		{"s05e14", `{"position_sec":300,"duration_sec":1300}`, "s05e14=300 s05e15=900 s05e16=900 s05e17=0"},
		{"s05e17", `{"position_sec":40,"duration_sec":1300}`, "s05e14=300 s05e15=900 s05e16=900 s05e17=40"},
	} {
		if w := send(h, "POST", "/api/v1/items/"+step.id+"/progress", viewer, step.body); w.Code != http.StatusNoContent {
			t.Fatalf("%s's progress: %d %s", step.id, w.Code, w.Body)
		}
		if got := positions(t, h, viewer, show...); got != step.want {
			t.Errorf("after a write for %s: %s, want %s", step.id, got, step.want)
		}
	}
	// A file of three: the episode in the middle writes each.
	if w := send(h, "POST", "/api/v1/items/s06e02/progress", viewer, `{"position_sec":120,"duration_sec":7200}`); w.Code != http.StatusNoContent {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := positions(t, h, viewer, saga...); got != "s06e01=120 s06e02=120 s06e03=120 s06e04=0" {
		t.Errorf("a file of three: %s", got)
	}

	// Watched, and unwatched, for each episode of the file alike.
	for _, step := range []struct {
		method, id, want string
	}{
		{"POST", "s05e16", "s05e15 s05e16"},
		{"POST", "s05e17", "s05e15 s05e16 s05e17"},
		{"DELETE", "s05e15", "s05e17"},
		{"POST", "s06e03", "s05e17 s06e01 s06e02 s06e03"},
		{"DELETE", "s06e01", "s05e17"},
	} {
		if w := do(h, step.method, "/api/v1/me/items/"+step.id+"/watched", viewer); w.Code != http.StatusNoContent {
			t.Fatalf("%s %s watched: %d %s", step.method, step.id, w.Code, w.Body)
		}
		if got := watchedOf(t, st, "viewer-1", append(show, saga...)...); got != step.want {
			t.Errorf("after %s %s watched: %q, want %q", step.method, step.id, got, step.want)
		}
	}

	// Reads stay the episode's own: a row written for one episode alone (as
	// before files held several) is read for it, and only for it.
	if err := st.SaveProgress(context.Background(), "viewer-1", []string{"s05e16"}, 1234, 2700); err != nil {
		t.Fatal(err)
	}
	if got := positions(t, h, viewer, "s05e15", "s05e16"); got != "s05e15=900 s05e16=1234" {
		t.Errorf("reads: %s", got)
	}

	// Another viewer's rows are their own.
	if got := positions(t, h, other, show...); got != "s05e14=0 s05e15=0 s05e16=0 s05e17=0" {
		t.Errorf("another viewer's progress: %s", got)
	}
	if got := watchedOf(t, st, "viewer-2", append(show, saga...)...); got != "" {
		t.Errorf("another viewer's watched: %q", got)
	}

	// Asked once in a while: writes for an episode whose file was asked for
	// a moment ago ask nothing.
	kat.requests()
	for _, id := range []string{"s05e16", "s05e16", "s05e15"} {
		if w := send(h, "POST", "/api/v1/items/"+id+"/progress", viewer, `{"position_sec":1000,"duration_sec":2700}`); w.Code != http.StatusNoContent {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
	if asked := kat.requests(); len(asked) != 0 {
		t.Errorf("asked again for a file known: %v", asked)
	}
	if got := positions(t, h, viewer, "s05e15", "s05e16"); got != "s05e15=1000 s05e16=1000" {
		t.Errorf("after the writes: %s", got)
	}

	// A capped viewer's write goes to the file too.
	kid := bearerWith(t, is, "kid-1", 12)
	if w := send(h, "POST", "/api/v1/items/s05e16/progress", kid, `{"position_sec":77,"duration_sec":2700}`); w.Code != http.StatusNoContent {
		t.Fatalf("a capped viewer: %d %s", w.Code, w.Body)
	}
	if got := positions(t, h, kid, show...); got != "s05e14=0 s05e15=77 s05e16=77 s05e17=0" {
		t.Errorf("a capped viewer: %s", got)
	}
}

// A katalog-api that names no covers, and one that does not answer, have
// each write go to the episode it is for alone, as before a file held
// several episodes: answered 204 all the same, and read back as written.
func TestWithoutAnAnswerOfTheFileAWriteIsTheEpisodesAlone(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	for i, tc := range []struct {
		name        string
		plain, down bool
	}{{"a katalog-api without covers", true, false}, {"a katalog-api down", false, true}} {
		kat := newFileCatalog(t, map[string][]string{"show": {"S05E14", "S05E15-E16", "S05E17"}})
		kat.set(tc.plain, tc.down)
		h := filesRouter(t, is, kat.URL, st)
		user := "viewer-" + strconv.Itoa(i)
		viewer := bearerWith(t, is, user, nil)
		if w := send(h, "POST", "/api/v1/items/s05e16/progress", viewer, `{"position_sec":600,"duration_sec":2700}`); w.Code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body)
		}
		if got := positions(t, h, viewer, "s05e15", "s05e16"); got != "s05e15=0 s05e16=600" {
			t.Errorf("%s, progress: %s", tc.name, got)
		}
		if w := do(h, "POST", "/api/v1/me/items/s05e15/watched", viewer); w.Code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body)
		}
		if got := watchedOf(t, st, user, "s05e15", "s05e16"); got != "s05e15" {
			t.Errorf("%s, watched: %q", tc.name, got)
		}
		if err := st.MarkWatched(context.Background(), user, "s05e16"); err != nil {
			t.Fatal(err)
		}
		if w := do(h, "DELETE", "/api/v1/me/items/s05e16/watched", viewer); w.Code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body)
		}
		if got := watchedOf(t, st, user, "s05e15", "s05e16"); got != "s05e15" {
			t.Errorf("%s, unwatched: %q", tc.name, got)
		}
	}
}

// asItems is eps as the katalog client reads them.
func asItems(t *testing.T, eps []fileEpisode) []katalog.Item {
	t.Helper()
	raw, err := json.Marshal(eps)
	if err != nil {
		t.Fatal(err)
	}
	var out []katalog.Item
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A file ends with the last of its episodes listed: the holder's and each
// covered episode's at the last of them, an episode with a file of its own at
// itself, whether katalog-api names the file on the holder (covers), on each
// covered episode (coveredBy) or on both. With neither, each episode is a
// file of its own.
func TestAFileEndsWithItsLastEpisode(t *testing.T) {
	eps := filed("show", "S05E14", "S05E15-E16", "S05E17-E19", "S05E20")
	holderOnly := append([]fileEpisode(nil), eps...)
	coveredOnly := append([]fileEpisode(nil), eps...)
	plain := append([]fileEpisode(nil), eps...)
	for i := range eps {
		holderOnly[i].CoveredBy = ""
		coveredOnly[i].Covers = nil
		plain[i].CoveredBy, plain[i].Covers = "", nil
	}
	for _, tc := range []struct {
		name string
		eps  []fileEpisode
		ends []int
	}{
		{"both", eps, []int{0, 2, 2, 5, 5, 5, 6}},
		{"on the holder", holderOnly, []int{0, 2, 2, 5, 5, 5, 6}},
		{"on the covered", coveredOnly, []int{0, 2, 2, 5, 5, 5, 6}},
		{"neither", plain, []int{0, 1, 2, 3, 4, 5, 6}},
	} {
		items := asItems(t, tc.eps)
		f := filesOf(items)
		for i, want := range tc.ends {
			if got := fileEnd(items, f, i); got != want {
				t.Errorf("%s: the file of %s ends at %d, want %d", tc.name, items[i].ID, got, want)
			}
		}
	}
}

// GET /api/v1/series/{id}/next-episode?after= is the episode after the
// anchor's file: after the last episode of a file that holds several, from
// its holder and from each episode it covers alike, so none of them is the
// next; the holder is the next of the episode before it, and the first of a
// series that begins with such a file. Inside season 0 a special's file is
// followed by the next special, or the first regular episode. A katalog-api
// that names no covers has each episode the next of the one before, as
// before.
func TestTheNextEpisodeIsAfterTheFile(t *testing.T) {
	kat := newFileCatalog(t, map[string][]string{
		"show": {"S05E14", "S05E15-E16", "S05E17-E19", "S05E20-E21"},
		"saga": {"S00E01-E02", "S01E01-E02", "S01E03"},
	})
	h := router(t, kat.URL, "http://katalog-manager.invalid", false)
	next := func(t *testing.T, series, after string) string {
		t.Helper()
		path := "/api/v1/series/" + series + "/next-episode"
		if after != "" {
			path += "?after=" + after
		}
		w := do(h, "GET", path, nil)
		var got struct {
			Next   *katalog.Item `json:"next"`
			Anchor string        `json:"anchor"`
			Reason string        `json:"reason"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		if got.Next == nil {
			return got.Reason
		}
		if after != "" && got.Anchor != after {
			t.Errorf("%s: anchor %q, want the episode asked about", path, got.Anchor)
		}
		return got.Next.ID
	}
	for _, tc := range []struct {
		series, after, files, plain string
	}{
		{"show", "s05e14", "s05e15", "s05e15"},
		{"show", "s05e15", "s05e17", "s05e16"},
		{"show", "s05e16", "s05e17", "s05e17"},
		{"show", "s05e17", "s05e20", "s05e18"},
		{"show", "s05e18", "s05e20", "s05e19"},
		{"show", "s05e19", "s05e20", "s05e20"},
		{"show", "s05e20", "end_of_series", "s05e21"},
		{"show", "s05e21", "end_of_series", "end_of_series"},
		{"saga", "", "s01e01", "s01e01"},
		{"saga", "s00e01", "s01e01", "s00e02"},
		{"saga", "s00e02", "s01e01", "s01e01"},
		{"saga", "s01e01", "s01e03", "s01e02"},
		{"saga", "s01e02", "s01e03", "s01e03"},
	} {
		kat.set(false, false)
		if got := next(t, tc.series, tc.after); got != tc.files {
			t.Errorf("%s after %q: %s, want %s", tc.series, tc.after, got, tc.files)
		}
		kat.set(true, false)
		if got := next(t, tc.series, tc.after); got != tc.plain {
			t.Errorf("%s after %q, a katalog-api without covers: %s, want %s", tc.series, tc.after, got, tc.plain)
		}
	}
}

// cwCard is a card of continue watching, as a client reads it.
type cwCard struct {
	ID          string `json:"id"`
	PositionSec int    `json:"position_sec"`
	UpNext      bool   `json:"up_next"`
	SeriesTitle string `json:"series_title"`
}

// continueCards is the viewer of header's continue watching.
func continueCards(t *testing.T, h http.Handler, header http.Header) []cwCard {
	t.Helper()
	w := do(h, "GET", "/api/v1/me/continue-watching", header)
	var body struct {
		Items []cwCard `json:"items"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("continue watching: %d %s", w.Code, w.Body)
	}
	return body.Items
}

// Continue watching has one card for the episodes of one file. While the
// file is unfinished, the holder's, with the position they share, after a
// write for any of them; once it is finished, the episode after the file's
// last - never one of the file - or after the next file that is not watched.
// An episode watched before its file was known to hold several stands for the
// file. The next episode of the series is after the file too. A katalog-api
// that names no covers has the rows of each episode cards as before.
func TestContinueWatchingHasOneCardForAFile(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	kat := newFileCatalog(t, map[string][]string{
		"show": {"S05E14", "S05E15-E16", "S05E17"},
		"saga": {"S06E01-E03", "S06E04"},
	})
	h := filesRouter(t, is, kat.URL, st)
	ctx := context.Background()
	card := func(c cwCard) string {
		s := c.ID + "@" + strconv.Itoa(c.PositionSec)
		if c.UpNext {
			s += " up next"
		}
		return s
	}
	cards := func(t *testing.T, h http.Handler, header http.Header) string {
		t.Helper()
		var out []string
		for _, c := range continueCards(t, h, header) {
			out = append(out, card(c))
			if c.SeriesTitle == "" {
				t.Errorf("%s: no series title", c.ID)
			}
		}
		return strings.Join(out, ", ")
	}
	post := func(t *testing.T, h http.Handler, header http.Header, id string, pos, dur int) {
		t.Helper()
		body := fmt.Sprintf(`{"position_sec":%d,"duration_sec":%d}`, pos, dur)
		if w := send(h, "POST", "/api/v1/items/"+id+"/progress", header, body); w.Code != http.StatusNoContent {
			t.Fatalf("%s's progress: %d %s", id, w.Code, w.Body)
		}
	}

	// Unfinished: the holder's card, from a write for the episode it covers.
	viewer := bearerWith(t, is, "viewer-1", nil)
	post(t, h, viewer, "s05e16", 600, 2700)
	if got := cards(t, h, viewer); got != "s05e15@600" {
		t.Errorf("an unfinished file: %q, want the holder's card", got)
	}
	// A file of three, from the episode in the middle; the most recent first.
	post(t, h, viewer, "s06e02", 100, 7200)
	if got := cards(t, h, viewer); got != "s06e01@100, s05e15@600" {
		t.Errorf("two unfinished files: %q", got)
	}

	// Finished, by a write for the covered episode and its watched: the
	// episode after the file.
	finished := bearerWith(t, is, "viewer-2", nil)
	post(t, h, finished, "s05e16", 2690, 2700)
	if w := do(h, "POST", "/api/v1/me/items/s05e16/watched", finished); w.Code != http.StatusNoContent {
		t.Fatalf("watched: %d %s", w.Code, w.Body)
	}
	if got := cards(t, h, finished); got != "s05e17@0 up next" {
		t.Errorf("a finished file: %q, want the episode after it", got)
	}
	w := do(h, "GET", "/api/v1/series/show/next-episode", finished)
	if !strings.Contains(w.Body.String(), `"next":{"id":"s05e17"`) {
		t.Errorf("the next episode after the file the viewer left: %d %s", w.Code, w.Body)
	}

	// Rows of the holder alone, written before its file was known to hold
	// two: the next is after the file all the same, not its covered episode.
	legacy := "viewer-3"
	if err := st.SaveProgress(ctx, legacy, []string{"s05e15"}, 2690, 2700); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkWatched(ctx, legacy, "s05e15"); err != nil {
		t.Fatal(err)
	}
	if got := cards(t, h, bearerWith(t, is, legacy, nil)); got != "s05e17@0 up next" {
		t.Errorf("a file watched as its holder alone: %q, want the episode after it", got)
	}

	// The walk past what is watched goes file by file: after S05E14 the file
	// S05E15-E16, watched as its holder alone, is passed whole.
	walk := "viewer-4"
	if err := st.SaveProgress(ctx, walk, []string{"s05e14"}, 1290, 1300); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkWatched(ctx, walk, "s05e15"); err != nil {
		t.Fatal(err)
	}
	if got := cards(t, h, bearerWith(t, is, walk, nil)); got != "s05e17@0 up next" {
		t.Errorf("after an episode, a watched file: %q, want the episode after the file", got)
	}
	// A finished file whose next episode is watched, the last of its series:
	// no card for it.
	if err := st.SaveProgress(ctx, walk, []string{"s06e01", "s06e02", "s06e03"}, 7190, 7200); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkWatched(ctx, walk, "s06e04"); err != nil {
		t.Fatal(err)
	}
	if got := cards(t, h, bearerWith(t, is, walk, nil)); got != "s05e17@0 up next" {
		t.Errorf("a series watched to its end: %q", got)
	}

	// A katalog-api without covers: each episode's rows are its own cards,
	// as before - the holder finished, its next the episode it covered.
	plain := newFileCatalog(t, map[string][]string{"show": {"S05E14", "S05E15-E16", "S05E17"}})
	plain.set(true, false)
	hp := filesRouter(t, is, plain.URL, st)
	if got := cards(t, hp, bearerWith(t, is, legacy, nil)); got != "s05e16@0 up next" {
		t.Errorf("a katalog-api without covers: %q, want the episode after the holder", got)
	}
}

// A covered episode plays, with nothing new on chino-api's side: its play
// routes take its id as they take any title's, behind the same gate, to
// chino-stream, which asks katalog-api where it plays from and is answered
// its holder's package, one package for the file. The master, a playlist and
// a segment a client gets for the covered episode, with a stream token minted
// as for any title (a capped viewer's too), are the holder's package's; the
// covered episode's own id reaches chino-stream unchanged, and a capped
// viewer's gate asks about it, not its holder.
func TestACoveredEpisodePlaysItsHoldersPackage(t *testing.T) {
	const holderDir = "series/5f/show/episodes/s05e15/versions/0b7e"
	kat := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/items/s05e16":
			_, _ = io.WriteString(w, `{"id":"s05e16","type":"episode","title":"Finale (2)","season_number":5,"episode_number":16,"parent_id":"show","coveredBy":"s05e15"}`)
		case "/api/v1/items/s05e15/playback", "/api/v1/items/s05e16/playback":
			// katalog-api's playback of a covered episode is its holder's,
			// and names the holder.
			id, holder := strings.Split(r.URL.Path, "/")[4], ""
			if id == "s05e16" {
				holder = `"coveredBy":"s05e15",`
			}
			_, _ = io.WriteString(w, `{"itemId":"`+id+`","type":"episode",`+holder+
				`"package":{"versionId":"0b7e","dir":"`+holderDir+`","record":"package.json"},"previous":[],"original":null}`)
		case "/api/v1/visible":
			_, _ = io.WriteString(w, `{"ids":["s05e15","s05e16"]}`)
		default:
			http.NotFound(w, r)
		}
	})
	// chino-stream, as far as this goes: what plays is the package katalog-api
	// names for the item.
	stream := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		id, route, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/play/"), "/")
		resp, err := http.Get(kat.URL + "/api/v1/items/" + id + "/playback")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		var pb struct {
			Package *struct {
				VersionID string `json:"versionId"`
				Dir       string `json:"dir"`
			} `json:"package"`
		}
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&pb) != nil || pb.Package == nil {
			http.NotFound(w, r)
			return
		}
		switch route {
		case "master.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=4000000,CODECS=\"hvc1.2.4.L120.B0,mp4a.40.2\"\n"+
				"v0/playlist.m3u8?v="+pb.Package.VersionID+"&"+r.URL.RawQuery+"\n## "+pb.Package.Dir+"\n")
		case "v0/playlist.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:6.0,\nseg-00001.m4s?"+r.URL.RawQuery+"\n## "+pb.Package.Dir+"\n")
		case "v0/seg-00001.m4s":
			_, _ = io.WriteString(w, "segment 1 of "+pb.Package.Dir)
		case "info":
			_, _ = io.WriteString(w, `{"duration_ms":5400000,"packaged":true}`)
		default:
			http.NotFound(w, r)
		}
	})
	is := newIssuer(t)
	h, err := NewRouter(config.Config{OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: kat.URL, StreamBaseURL: stream.URL, ArtworkBaseURL: "http://katalog-manager.invalid",
		StreamSigningKey: signingKey}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		bearer http.Header
		gate   string // what katalog-api is asked of the cap, once in its time
	}{
		{"a viewer", bearerWith(t, is, "viewer-1", nil), ""},
		{"a viewer capped at 12", bearerWith(t, is, "kid-1", 12), "GET /api/v1/visible?ids=s05e16&max_rating=12"},
	} {
		kat.take()
		w := do(h, "POST", "/api/v1/me/stream-token", tc.bearer)
		var minted struct {
			Token string `json:"stream_token"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &minted) != nil || minted.Token == "" {
			t.Fatalf("%s, the stream token: %d %s", tc.name, w.Code, w.Body)
		}
		w = do(h, "GET", "/api/v1/items/s05e16", tc.bearer)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"coveredBy":"s05e15"`) {
			t.Fatalf("%s, the covered episode's detail: %d %s", tc.name, w.Code, w.Body)
		}
		stream.take()

		q := "?stream=" + minted.Token + "&caps=hvc:2160,aac"
		for _, step := range []struct {
			route, want string
		}{
			{"master.m3u8", "v0/playlist.m3u8?v=0b7e&stream=" + minted.Token},
			{"v0/playlist.m3u8", "seg-00001.m4s?stream=" + minted.Token},
			{"v0/seg-00001.m4s", "segment 1 of " + holderDir},
			{"info", `"packaged":true`},
		} {
			w := do(h, "GET", "/api/v1/items/s05e16/play/"+step.route+q, nil)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), step.want) ||
				(strings.HasSuffix(step.route, ".m3u8") && !strings.Contains(w.Body.String(), holderDir)) {
				t.Errorf("%s, the covered episode's %s: %d %q, want the holder's package", tc.name, step.route, w.Code, w.Body)
			}
		}
		got := stream.take()
		if len(got) != 4 || !strings.HasPrefix(got[0], "GET /api/play/s05e16/master.m3u8?") || !strings.HasPrefix(got[2], "GET /api/play/s05e16/v0/seg-00001.m4s?") {
			t.Errorf("%s: chino-stream got %q, want the covered episode's id", tc.name, got)
		}
		var gate []string
		for _, r := range kat.take() {
			if strings.Contains(r, "/api/v1/visible") {
				gate = append(gate, r)
			}
		}
		if tc.gate == "" && len(gate) != 0 || tc.gate != "" && (len(gate) != 1 || gate[0] != tc.gate) {
			t.Errorf("%s: katalog-api was asked %q, want %q", tc.name, gate, tc.gate)
		}
	}
}
