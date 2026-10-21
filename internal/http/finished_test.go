package http

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A title saved in its finished end - its last 5 % or 2 minutes, whichever
// is longer (store.Finished) - counts as watched and has no resume point,
// whichever client saved it; an episode's next takes over in Continue
// Watching, with that one's own resume point where it has one.
func TestAFinishedTitleIsWatchedAndHasNoResumePoint(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	// Episodes of 22 min: their last 2 min finish them.
	kat := newFileCatalog(t, map[string][]string{"show": {"S01E01", "S01E02", "S01E03"}})
	h := filesRouter(t, is, kat.URL, st)
	ctx := context.Background()

	post := func(t *testing.T, header http.Header, id string, pos, dur int, extra ...string) {
		t.Helper()
		body := fmt.Sprintf(`{"position_sec":%d,"duration_sec":%d%s}`, pos, dur, strings.Join(extra, ""))
		if w := send(h, "POST", "/api/v1/items/"+id+"/progress", header, body); w.Code != http.StatusNoContent {
			t.Fatalf("%s's progress: %d %s", id, w.Code, w.Body)
		}
	}
	resume := func(t *testing.T, header http.Header, id string) string {
		t.Helper()
		w := do(h, "GET", "/api/v1/items/"+id+"/progress", header)
		if w.Code != http.StatusOK {
			t.Fatalf("%s's progress: %d %s", id, w.Code, w.Body)
		}
		return strings.TrimSpace(w.Body.String())
	}
	watched := func(t *testing.T, sub string, ids ...string) string {
		t.Helper()
		got, err := st.WatchedAtBatch(ctx, sub, ids)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, id := range ids {
			if _, ok := got[id]; ok {
				out = append(out, id)
			}
		}
		return strings.Join(out, " ")
	}
	cards := func(t *testing.T, header http.Header) string {
		t.Helper()
		var out []string
		for _, c := range continueCards(t, h, header) {
			s := fmt.Sprintf("%s@%d", c.ID, c.PositionSec)
			if c.UpNext {
				s += " up next"
			}
			out = append(out, s)
		}
		return strings.Join(out, ", ")
	}

	// A film of 2 h: in its last 5 % (6 min) it is finished, before them not.
	film := bearerWith(t, is, "film-viewer", nil)
	post(t, film, "film", 3600, 7200)
	if got := resume(t, film, "film"); got != `{"position_sec":3600}` {
		t.Errorf("a film in its middle: %s, want its position", got)
	}
	if got := watched(t, "film-viewer", "film"); got != "" {
		t.Errorf("a film in its middle is marked watched")
	}
	post(t, film, "film", 6840, 7200)
	if got := resume(t, film, "film"); got != `{"finished":true,"position_sec":0}` {
		t.Errorf("a film 6 min before its end: %s, want no resume point", got)
	}
	if got := watched(t, "film-viewer", "film"); got != "film" {
		t.Errorf("a film 6 min before its end is not marked watched")
	}

	// An episode 1 min 10 s before its end - past the 60 s the rows used to
	// be finished by, within its last 2 min: finished, watched, and its next
	// one up next, from the start.
	viewer := bearerWith(t, is, "viewer-1", nil)
	post(t, viewer, "s01e01", 1250, 1320)
	if got := resume(t, viewer, "s01e01"); got != `{"finished":true,"position_sec":0}` {
		t.Errorf("an episode in its last 2 min: %s, want no resume point", got)
	}
	if got := watched(t, "viewer-1", "s01e01"); got != "s01e01" {
		t.Errorf("an episode in its last 2 min is not marked watched")
	}
	if got := cards(t, viewer); got != "s01e02@0 up next" {
		t.Errorf("after a finished episode: %q, want the next one up next", got)
	}

	// The next one's own resume point takes over: the viewer was in its
	// middle before they watched the one before it to its end.
	again := bearerWith(t, is, "viewer-2", nil)
	post(t, again, "s01e02", 600, 1320)
	post(t, again, "s01e01", 1250, 1320)
	if got := cards(t, again); got != "s01e02@600" {
		t.Errorf("after a finished episode whose next was begun: %q, want that one at its position", got)
	}
	// A title taken off Continue Watching - saved at its end with dismiss -
	// is gone from it, has no resume point, and is not marked watched: the
	// viewer did not finish it. Its next episode is up next.
	dismissed := bearerWith(t, is, "viewer-4", nil)
	post(t, dismissed, "s01e01", 600, 1320)
	post(t, dismissed, "s01e01", 1320, 1320, `,"dismiss":true`)
	if got := watched(t, "viewer-4", "s01e01"); got != "" {
		t.Errorf("a dismissed episode is marked watched")
	}
	if got := resume(t, dismissed, "s01e01"); got != `{"finished":true,"position_sec":0}` {
		t.Errorf("a dismissed episode: %s, want no resume point", got)
	}
	if got := cards(t, dismissed); got != "s01e02@0 up next" {
		t.Errorf("after a dismissed episode: %q, want the next one up next", got)
	}
	post(t, dismissed, "film", 1000, 7200)
	post(t, dismissed, "film", 7200, 7200, `,"dismiss":true`)
	if got := watched(t, "viewer-4", "film"); got != "" {
		t.Errorf("a dismissed film is marked watched")
	}

	// The next one's own end, finished too, is no resume point: the one after
	// it is up next.
	done := bearerWith(t, is, "viewer-3", nil)
	post(t, done, "s01e02", 1300, 1320)
	post(t, done, "s01e01", 1250, 1320)
	if got := cards(t, done); got != "s01e03@0 up next" {
		t.Errorf("after two finished episodes: %q, want the third up next", got)
	}
}
