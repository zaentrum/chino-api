package store

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// progressOf is userID's progress rows, "item position/duration" in item
// order.
func progressOf(t *testing.T, st *Store, userID string) string {
	t.Helper()
	rows, err := st.p.Query(context.Background(),
		`SELECT item_id, position_sec, duration_sec FROM playback_progress WHERE user_id = $1 ORDER BY item_id`, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		var pos, dur int
		if err := rows.Scan(&id, &pos, &dur); err != nil {
			t.Fatal(err)
		}
		out = append(out, id+" "+strconv.Itoa(pos)+"/"+strconv.Itoa(dur))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, ", ")
}

// watchedOf is the items userID has watched, in item order.
func watchedOf(t *testing.T, st *Store, userID string) string {
	t.Helper()
	got, err := st.WatchedAtBatch(context.Background(), userID, []string{"e15", "e16", "e17", "m1"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for id := range got {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, " ")
}

// Progress and watched are written for every item a write names - the
// episodes of one file - in one statement: the same position for each, at
// the same time, each duration the longest it was told; each item once,
// whatever repeats or empty ids the write names. Reads stay per item, and
// another person's rows are not touched.
func TestProgressAndWatchedOfSeveralItems(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.SaveProgress(ctx, "other-1", []string{"e15"}, 99, 100); err != nil {
		t.Fatal(err)
	}

	if err := st.SaveProgress(ctx, "kid-1", []string{"e15", "e16", "e15", ""}, 600, 5400); err != nil {
		t.Fatal(err)
	}
	if got := progressOf(t, st, "kid-1"); got != "e15 600/5400, e16 600/5400" {
		t.Errorf("one file's two episodes: %q", got)
	}
	var stamps int
	if err := st.p.QueryRow(ctx, `SELECT count(DISTINCT updated_at) FROM playback_progress WHERE user_id = 'kid-1'`).Scan(&stamps); err != nil || stamps != 1 {
		t.Errorf("written at %d times (%v), want one", stamps, err)
	}
	if err := st.SaveProgress(ctx, "kid-1", []string{"e15"}, 700, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveProgress(ctx, "kid-1", []string{"e15", "e16", "e17"}, -5, 3000); err != nil {
		t.Fatal(err)
	}
	if got := progressOf(t, st, "kid-1"); got != "e15 0/5400, e16 0/5400, e17 0/3000" {
		t.Errorf("then three, at a position below 0: %q", got)
	}
	if err := st.SaveProgress(ctx, "kid-1", []string{"e15", "e16", "e17"}, 1200, 5400); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int{"e15": 1200, "e16": 1200, "e17": 1200, "m1": 0} {
		if pos, err := st.GetProgress(ctx, "kid-1", id); err != nil || pos != want {
			t.Errorf("%s read alone: %d %v, want %d", id, pos, err, want)
		}
	}
	if got := progressOf(t, st, "other-1"); got != "e15 99/100" {
		t.Errorf("another person's progress: %q", got)
	}

	if err := st.MarkWatched(ctx, "kid-1", "e15", "e16", "e16", ""); err != nil {
		t.Fatal(err)
	}
	if got := watchedOf(t, st, "kid-1"); got != "e15 e16" {
		t.Errorf("watched: %q", got)
	}
	before, _ := st.WatchedAtBatch(ctx, "kid-1", []string{"e15", "e16"})
	time.Sleep(10 * time.Millisecond)
	if err := st.MarkWatched(ctx, "kid-1", "e15", "e16", "e17"); err != nil {
		t.Fatal(err)
	}
	after, _ := st.WatchedAtBatch(ctx, "kid-1", []string{"e15", "e16", "e17"})
	if !after["e15"].After(before["e15"]) || !after["e16"].After(before["e16"]) ||
		!after["e15"].Equal(after["e16"]) || !after["e16"].Equal(after["e17"]) {
		t.Errorf("watched again: %v, before %v; want each bumped, to one time", after, before)
	}
	if err := st.MarkWatched(ctx, "other-1", "e15"); err != nil {
		t.Fatal(err)
	}
	if err := st.UnmarkWatched(ctx, "kid-1", "e15", "e16", "e17", "m1"); err != nil {
		t.Fatal(err)
	}
	if got := watchedOf(t, st, "kid-1"); got != "" {
		t.Errorf("unwatched: %q left", got)
	}
	if got := watchedOf(t, st, "other-1"); got != "e15" {
		t.Errorf("another person's watched: %q", got)
	}

	// Nothing named, or no database: nothing written, and no error.
	for _, err := range []error{
		st.SaveProgress(ctx, "kid-1", nil, 10, 20), st.MarkWatched(ctx, "kid-1"), st.UnmarkWatched(ctx, "kid-1", ""),
	} {
		if err != nil {
			t.Errorf("a write of nothing: %v", err)
		}
	}
	var none *Store
	if err := none.SaveProgress(ctx, "kid-1", []string{"e15"}, 10, 20); err != nil {
		t.Errorf("no store: %v", err)
	}
	if err := none.MarkWatched(ctx, "kid-1", "e15"); err != nil {
		t.Errorf("no store: %v", err)
	}
}
