package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/chino-api/internal/katalog"
)

// episodes is a katalog-api /series/{id}/episodes body: one episode per
// "SxxEyy" code (S-- = no season number), in the order given, ids "sxxeyy".
func episodes(codes ...string) string {
	var items []string
	for _, c := range codes {
		season := "null"
		if c[1:3] != "--" {
			season = strings.TrimLeft(c[1:3], "0")
			if season == "" {
				season = "0"
			}
		}
		items = append(items, fmt.Sprintf(`{"id":%q,"type":"episode","title":%q,"season_number":%s,"episode_number":%s}`,
			strings.ToLower(c), c, season, strings.TrimLeft(c[4:], "0")))
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

func TestFirstEpisodeAndEpisodeAfterLeaveSpecialsOut(t *testing.T) {
	list := func(codes ...string) []katalog.Item {
		var body struct{ Items []katalog.Item }
		if err := json.Unmarshal([]byte(episodes(codes...)), &body); err != nil {
			t.Fatal(err)
		}
		return body.Items
	}
	eps := list("S00E01", "S00E02", "S01E01", "S01E02", "S02E01")
	if i := firstEpisode(eps); i != 2 {
		t.Errorf("firstEpisode = %d, want 2 (S01E01, not the special before it)", i)
	}
	if i := firstEpisode(list("S00E01", "S00E02")); i != 0 {
		t.Errorf("a series of specials only begins with its first: %d", i)
	}
	if i := firstEpisode(list("S00E01", "S--E01")); i != 1 {
		t.Errorf("an episode without a season number is not a special: %d", i)
	}
	cases := []struct {
		eps    []katalog.Item
		anchor int
		want   int
	}{
		{eps, 0, 1},  // inside season 0: the next special
		{eps, 1, 2},  // the last special: on to S01E01
		{eps, 2, 3},  // S01E01 → S01E02
		{eps, 3, 4},  // S01E02 → S02E01
		{eps, 4, -1}, // end of the series
		// specials listed between seasons are skipped from a regular episode
		{list("S01E01", "S00E01", "S01E02"), 0, 2},
		{list("S01E01", "S00E01"), 0, -1},
		{list("S01E01", "S--E01"), 0, 1},
	}
	for _, tc := range cases {
		if got := episodeAfter(tc.eps, tc.anchor); got != tc.want {
			t.Errorf("episodeAfter(%s) = %d, want %d", tc.eps[tc.anchor].Title, got, tc.want)
		}
	}
}

// GET /api/v1/series/{id}/next-episode: a series nobody has started begins
// with S01E01, not the special katalog lists first; from inside season 0
// the specials follow each other.
func TestNextEpisodeSkipsSpecialsUnlessInsideSeasonZero(t *testing.T) {
	kat := newFake(t, map[string]string{
		"/api/v1/series/s1/episodes": episodes("S00E01", "S00E02", "S01E01", "S01E02"),
		"/api/v1/series/s2/episodes": episodes("S00E01"),
		"/api/v1/series/s3/episodes": episodes("S01E01", "S00E01", "S01E02"),
	})
	h := router(t, kat.URL, "http://katalog-manager.invalid", false)
	cases := []struct {
		path, next, anchor string
	}{
		{"/api/v1/series/s1/next-episode", "s01e01", ""},
		{"/api/v1/series/s1/next-episode?after=s00e01", "s00e02", "s00e01"},
		{"/api/v1/series/s1/next-episode?after=s00e02", "s01e01", "s00e02"},
		{"/api/v1/series/s1/next-episode?after=s01e01", "s01e02", "s01e01"},
		{"/api/v1/series/s1/next-episode?after=s01e02", "", ""},
		{"/api/v1/series/s2/next-episode", "s00e01", ""},
		// a special listed between regular episodes is not their next
		{"/api/v1/series/s3/next-episode?after=s01e01", "s01e02", "s01e01"},
	}
	for _, tc := range cases {
		w := do(h, "GET", tc.path, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body)
		}
		var got struct {
			Next   *katalog.Item `json:"next"`
			Anchor string        `json:"anchor"`
			Reason string        `json:"reason"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		switch {
		case tc.next == "" && (got.Next != nil || got.Reason != "end_of_series"):
			t.Errorf("%s: %s, want the end of the series", tc.path, w.Body)
		case tc.next != "" && (got.Next == nil || got.Next.ID != tc.next || got.Anchor != tc.anchor):
			t.Errorf("%s: %s, want next %s (anchor %q)", tc.path, w.Body, tc.next, tc.anchor)
		}
	}
}
