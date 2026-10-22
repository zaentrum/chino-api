package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
)

// A position a phone kept without a connection comes in with its age: it is
// written only over an older one, and marks the title watched only where it
// was written - the phone's earlier second never takes back what another
// device played since. A position without its age is written now, over any,
// as every other client's.
func TestALatePositionMarksWatchedOnlyWhereItWasWritten(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	katalog := ratedKatalog(t)
	h, err := NewRouter(config.Config{OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: katalog.URL, StreamBaseURL: "http://chino-stream.invalid",
		ArtworkBaseURL: "http://katalog-manager.invalid", StreamSigningKey: signingKey}, st, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	viewer := bearerWith(t, is, "anna", nil)
	post := func(body string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/items/kid/progress", strings.NewReader(body))
		r.Header = viewer.Clone()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("POST %s: %d %s", body, w.Code, w.Body)
		}
	}
	position := func() (int, bool) {
		t.Helper()
		w := do(h, http.MethodGet, "/api/v1/items/kid/progress", viewer)
		var got struct {
			PositionSec int  `json:"position_sec"`
			Finished    bool `json:"finished"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatalf("GET progress: %d %s", w.Code, w.Body)
		}
		return got.PositionSec, got.Finished
	}
	watched := func() bool {
		t.Helper()
		at, err := st.WatchedAtBatch(context.Background(), "anna", []string{"kid"})
		if err != nil {
			t.Fatal(err)
		}
		_, ok := at["kid"]
		return ok
	}

	// Played on the phone three hours ago, to 10:00.
	post(`{"position_sec":600,"duration_sec":5400,"played_ms_ago":10800000}`)
	// The phone's finish of four hours ago is older: it loses, nothing is watched.
	post(`{"position_sec":5350,"duration_sec":5400,"played_ms_ago":14400000}`)
	if pos, _ := position(); pos != 600 || watched() {
		t.Errorf("an older finish: at %d, watched %v; want 600, not watched", pos, watched())
	}
	// Its finish of two hours ago is newer: written, and watched.
	post(`{"position_sec":5350,"duration_sec":5400,"played_ms_ago":7200000}`)
	if _, finished := position(); !finished || !watched() {
		t.Errorf("a newer finish: finished %v, watched %v", finished, watched())
	}
	// The TV starts it over, now, without an age: written as before.
	post(`{"position_sec":120,"duration_sec":5400}`)
	if pos, _ := position(); pos != 120 {
		t.Errorf("a write without an age: at %d, want 120", pos)
	}
	// The phone's finish of an hour ago comes in late: the TV's rewatch stays.
	post(`{"position_sec":5350,"duration_sec":5400,"played_ms_ago":3600000}`)
	if pos, _ := position(); pos != 120 {
		t.Errorf("a late finish over a newer rewatch: at %d, want 120", pos)
	}
	// An age below zero is now's.
	post(`{"position_sec":300,"duration_sec":5400,"played_ms_ago":-5000}`)
	if pos, _ := position(); pos != 300 {
		t.Errorf("an age below zero: at %d, want 300", pos)
	}
}
