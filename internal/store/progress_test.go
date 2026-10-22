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

// A position played some time ago - kept on a phone while it had no
// connection - is written only where the row is older: a newer position
// another device wrote meanwhile stays. Each row of a file is decided on its
// own, a title with no row yet takes it, and the write says where it won. A
// write without an age is now's, over any row, as before.
func TestALatePositionDoesNotOverwriteANewerOne(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	// The TV finished the film just now.
	if err := st.SaveProgress(ctx, "anna", []string{"m1"}, 5300, 5400); err != nil {
		t.Fatal(err)
	}
	// The phone's position of two hours ago comes in.
	won, err := st.SaveProgressPlayed(ctx, "anna", []string{"m1"}, 45, 5400, 2*time.Hour)
	if err != nil || len(won) != 0 {
		t.Errorf("an older position over a newer one: won %v (%v)", won, err)
	}
	if got := progressOf(t, st, "anna"); got != "m1 5300/5400" {
		t.Errorf("the newer position: %q", got)
	}

	// Where the row is three hours old, a position of a minute ago is
	// written, at the time it was played.
	if _, err := st.p.Exec(ctx, `UPDATE playback_progress SET updated_at = now() - interval '3 hours' WHERE user_id = 'anna'`); err != nil {
		t.Fatal(err)
	}
	won, err = st.SaveProgressPlayed(ctx, "anna", []string{"m1"}, 1200, 5400, time.Minute)
	if err != nil || strings.Join(won, " ") != "m1" {
		t.Errorf("a newer position over an older one: won %v (%v)", won, err)
	}
	if got := progressOf(t, st, "anna"); got != "m1 1200/5400" {
		t.Errorf("after the newer position: %q", got)
	}
	var ageSec float64
	if err := st.p.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM now() - updated_at) FROM playback_progress WHERE user_id = 'anna' AND item_id = 'm1'`).Scan(&ageSec); err != nil || ageSec < 50 || ageSec > 70 {
		t.Errorf("written %v s ago (%v), want about 60", ageSec, err)
	}

	// The episodes of one file: one has a newer row, the other none.
	if err := st.SaveProgress(ctx, "anna", []string{"e16"}, 300, 1800); err != nil {
		t.Fatal(err)
	}
	won, err = st.SaveProgressPlayed(ctx, "anna", []string{"e15", "e16", "e15"}, 600, 1800, 10*time.Minute)
	if err != nil || strings.Join(won, " ") != "e15" {
		t.Errorf("one file's episodes: won %v (%v), want e15", won, err)
	}
	if got := progressOf(t, st, "anna"); got != "e15 600/1800, e16 300/1800, m1 1200/5400" {
		t.Errorf("one file's episodes: %q", got)
	}

	// Without an age, as before: now's, whatever the row says.
	if err := st.SaveProgress(ctx, "anna", []string{"e16"}, 30, 1800); err != nil {
		t.Fatal(err)
	}
	if got := progressOf(t, st, "anna"); !strings.Contains(got, "e16 30/1800") {
		t.Errorf("a write without an age: %q", got)
	}
	// Another person's rows are their own.
	if won, err := st.SaveProgressPlayed(ctx, "ben", []string{"m1"}, 99, 5400, time.Hour); err != nil || strings.Join(won, " ") != "m1" {
		t.Errorf("another person's first position: won %v (%v)", won, err)
	}
	if got := progressOf(t, st, "anna"); !strings.Contains(got, "m1 1200/5400") {
		t.Errorf("anna's row after ben's write: %q", got)
	}
}

// A title finished in a late write is watched when it was played, or later
// where it was watched again since.
func TestALateFinishIsWatchedWhenItWasPlayed(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	// How long ago m1 was watched, as the database counts it.
	watchedAgo := func() time.Duration {
		t.Helper()
		var sec float64
		if err := st.p.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM now() - watched_at) FROM watched_history WHERE user_id = 'anna' AND item_id = 'm1'`).Scan(&sec); err != nil {
			t.Fatal(err)
		}
		return time.Duration(sec * float64(time.Second))
	}
	if err := st.MarkWatchedPlayed(ctx, "anna", 2*time.Hour, "m1"); err != nil {
		t.Fatal(err)
	}
	if ago := watchedAgo(); ago < 119*time.Minute || ago > 121*time.Minute {
		t.Errorf("watched %v ago, want two hours", ago)
	}
	if err := st.MarkWatched(ctx, "anna", "m1"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkWatchedPlayed(ctx, "anna", 3*time.Hour, "m1"); err != nil {
		t.Fatal(err)
	}
	if ago := watchedAgo(); ago > time.Minute {
		t.Errorf("watched again since: %v ago, want now", ago)
	}
}
