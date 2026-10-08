package katalog

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/chino-api/internal/auth"
)

// episodeCatalog is a katalog-api answering the items of a series whose
// S05E15-E17 is one file (holder e15), beside episodes and a movie of a file
// of their own, a covered episode whose holder it does not serve (e30), and
// one its holder does not list (e40). It counts how often each item was
// asked for, and keeps the cap of each request.
type episodeCatalog struct {
	*httptest.Server
	mu    sync.Mutex
	asked map[string]int
	caps  []string
	fail  bool
}

func newEpisodeCatalog(t *testing.T) *episodeCatalog {
	t.Helper()
	items := map[string]string{
		"e14": `{"id":"e14","type":"episode","title":"Before","season_number":5,"episode_number":14,"parent_id":"s1"}`,
		"e15": `{"id":"e15","type":"episode","title":"Finale (1)","season_number":5,"episode_number":15,"parent_id":"s1","covers":["e16","e17"],"episodeEnd":17}`,
		"e16": `{"id":"e16","type":"episode","title":"Finale (2)","season_number":5,"episode_number":16,"parent_id":"s1","coveredBy":"e15"}`,
		"e17": `{"id":"e17","type":"episode","title":"Finale (3)","season_number":5,"episode_number":17,"parent_id":"s1","coveredBy":"e15"}`,
		"e30": `{"id":"e30","type":"episode","title":"Hidden holder's","season_number":6,"episode_number":2,"parent_id":"s1","coveredBy":"e29"}`,
		"e39": `{"id":"e39","type":"episode","title":"Holder","season_number":7,"episode_number":1,"parent_id":"s1","covers":["e41"],"episodeEnd":3}`,
		"e40": `{"id":"e40","type":"episode","title":"Unlisted","season_number":7,"episode_number":2,"parent_id":"s1","coveredBy":"e39"}`,
		"m1":  `{"id":"m1","type":"movie","title":"A Film"}`,
	}
	c := &episodeCatalog{asked: map[string]int{}}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/items/")
		c.asked[id]++
		c.caps = append(c.caps, r.URL.Query().Get("max_rating")+" "+r.Header.Get("Authorization"))
		if c.fail {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		body, ok := items[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(c.Close)
	return c
}

// take is how often each item was asked for, and the caps and bearers of
// the requests, since the last take.
func (c *episodeCatalog) take() (map[string]int, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	asked, caps := c.asked, c.caps
	c.asked, c.caps = map[string]int{}, nil
	return asked, caps
}

// FileEpisodes is the episodes of the file an item plays, the holder first
// and the others in episode order, the same from each of them: the holder's
// own covers, or a covered episode's holder's. An item with a file of its own
// (an episode, a movie), and one katalog-api does not know, is its file
// alone; a covered episode whose holder katalog-api does not serve shares the
// holder's file all the same, and one its holder does not list is in it too.
// Each answer is asked for with the viewer's bearer and cap, kept a while per
// cap: asked again in its time, it is not asked again; another cap asks anew.
// A failure is an error, kept for nothing; an answer past its time is asked
// anew.
func TestFileEpisodesAsksOnceInAWhile(t *testing.T) {
	cat := newEpisodeCatalog(t)
	c := New(cat.URL)
	ctx := context.Background()
	want := map[string]string{
		"e15": "e15 e16 e17", "e16": "e15 e16 e17", "e17": "e15 e16 e17",
		"e14": "e14", "m1": "m1", "nothing": "nothing", "e30": "e29 e30", "e40": "e39 e41 e40",
	}
	for round := 0; round < 2; round++ {
		for id, w := range want {
			got, err := c.FileEpisodes(ctx, "tok", id)
			if err != nil || strings.Join(got, " ") != w {
				t.Errorf("%s: %q %v, want %q", id, got, err, w)
			}
		}
		asked, caps := cat.take()
		if round == 0 {
			// Each item once, and a covered episode's holder for each episode
			// it covers.
			if asked["e15"] != 3 || asked["e16"] != 1 || asked["e14"] != 1 || asked["nothing"] != 1 || asked["e29"] != 1 || asked["e39"] != 1 {
				t.Errorf("asked %v", asked)
			}
			for _, sent := range caps {
				if sent != " Bearer tok" {
					t.Errorf("an uncapped viewer's request went with the cap and bearer %q", sent)
				}
			}
		} else if len(asked) != 0 {
			t.Errorf("asked again within the time: %v", asked)
		}
	}
	// What is given out is the caller's to change.
	got, _ := c.FileEpisodes(ctx, "tok", "e15")
	got[0] = "changed"
	if got, _ := c.FileEpisodes(ctx, "tok", "e15"); got[0] != "e15" {
		t.Errorf("a kept answer was changed: %q", got)
	}

	capped := auth.WithMaxRating(ctx, 12)
	if got, err := c.FileEpisodes(capped, "tok", "e16"); err != nil || strings.Join(got, " ") != "e15 e16 e17" {
		t.Errorf("capped at 12: %q %v", got, err)
	}
	if asked, caps := cat.take(); asked["e16"] != 1 || asked["e15"] != 1 || len(caps) != 2 || caps[0] != "12 Bearer tok" {
		t.Errorf("another cap: asked %v with %q", asked, caps)
	}

	cat.mu.Lock()
	cat.fail = true
	cat.mu.Unlock()
	if got, err := c.FileEpisodes(ctx, "tok", "e99"); err == nil {
		t.Errorf("katalog-api failing: %q, no error", got)
	}
	if got, err := c.FileEpisodes(ctx, "tok", "e16"); err != nil || strings.Join(got, " ") != "e15 e16 e17" {
		t.Errorf("a file known within its time, katalog-api failing: %q %v", got, err)
	}
	cat.mu.Lock()
	cat.fail = false
	cat.mu.Unlock()
	cat.take()
	if got, err := c.FileEpisodes(ctx, "tok", "e99"); err != nil || strings.Join(got, " ") != "e99" {
		t.Errorf("after the failure: %q %v", got, err)
	}
	if asked, _ := cat.take(); asked["e99"] != 1 {
		t.Errorf("the failure was kept: asked %v", asked)
	}

	// Kept for fileTTL only.
	c.visible.mu.Lock()
	for k, a := range c.visible.file {
		a.until = time.Now().Add(-time.Second)
		c.visible.file[k] = a
	}
	c.visible.mu.Unlock()
	if _, err := c.FileEpisodes(ctx, "tok", "e16"); err != nil {
		t.Fatal(err)
	}
	if asked, _ := cat.take(); asked["e16"] != 1 || asked["e15"] != 1 {
		t.Errorf("an answer past its time: asked %v", asked)
	}
}
