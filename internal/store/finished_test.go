package store

import (
	"context"
	"strconv"
	"testing"
)

// A title is finished in its last 5 % or its last 2 minutes, whichever is
// longer, never more than its second half; one of unknown length never.
func TestFinished(t *testing.T) {
	cases := []struct {
		name          string
		position, dur int
		want          bool
	}{
		// A film of 2 h: its last 5 % (6 min) is longer than 2 min.
		{"2 h film, 6 min before its end", 6840, 7200, true},
		{"2 h film, 6 min and 1 s before its end", 6839, 7200, false},
		{"2 h film at its end", 7200, 7200, true},
		{"2 h film past its saved end", 7300, 7200, true},
		// An episode of 22 min: its last 2 min are longer than its last 5 %.
		{"22 min episode, 2 min before its end", 1200, 1320, true},
		{"22 min episode, 2 min and 1 s before its end", 1199, 1320, false},
		// 40 min: 5 % is 2 min, the two the same.
		{"40 min, 2 min before its end", 2280, 2400, true},
		{"40 min, 2 min and 1 s before its end", 2279, 2400, false},
		// A short of 146 s: never more than its second half (73 s).
		{"146 s short at its half", 73, 146, true},
		{"146 s short before its half", 72, 146, false},
		{"146 s short at its first second", 1, 146, false},
		// Unknown length.
		{"no length", 5000, 0, false},
		{"no length, no position", 0, 0, false},
	}
	for _, c := range cases {
		if got := Finished(c.position, c.dur); got != c.want {
			t.Errorf("%s: Finished(%d, %d) = %v, want %v", c.name, c.position, c.dur, got, c.want)
		}
	}
}

// The database decides finished as Finished does, at each second around the
// end of titles short and long; GET progress has no resume point for one,
// and Continue Watching flags it finished.
func TestTheDatabaseFinishesAsFinishedDoes(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	user := "viewer-1"
	type row struct {
		id            string
		position, dur int
	}
	var rows []row
	n := 0
	for _, dur := range []int{90, 146, 1320, 2400, 7200} {
		end := dur - finishedTail(dur)
		for _, pos := range []int{end - 2, end - 1, end, end + 1, dur} {
			if pos <= MinResumeSec {
				continue
			}
			n++
			rows = append(rows, row{id: "item-" + strconv.Itoa(n), position: pos, dur: dur})
		}
	}
	for _, r := range rows {
		if err := st.SaveProgress(ctx, user, []string{r.id}, r.position, r.dur); err != nil {
			t.Fatal(err)
		}
	}
	cw, err := st.ListContinueWatching(ctx, user, 100)
	if err != nil {
		t.Fatal(err)
	}
	flagged := map[string]bool{}
	for _, p := range cw {
		flagged[p.ItemID] = p.Finished
	}
	for _, r := range rows {
		want := Finished(r.position, r.dur)
		if got, ok := flagged[r.id]; !ok || got != want {
			t.Errorf("%d of %d s: the database says finished %v (listed %v), Finished %v", r.position, r.dur, got, ok, want)
		}
		pos, err := st.GetProgress(ctx, user, r.id)
		if err != nil {
			t.Fatal(err)
		}
		if want && pos != 0 {
			t.Errorf("%d of %d s: finished, but its resume point is %d", r.position, r.dur, pos)
		}
		if !want && pos != r.position {
			t.Errorf("%d of %d s: its resume point is %d, want the position", r.position, r.dur, pos)
		}
	}

}
