package store

import (
	"context"
	"testing"
)

// An account's settings are its own: the first values until it sets one, a
// change of nothing reads them, and another person's are theirs.
func Test16C25TheAccountKeepsItsSettings(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	on, off := true, false
	for _, step := range []struct {
		name   string
		user   string
		change SettingsChange
		want   Settings
	}{
		{"at first", "viewer-1", SettingsChange{}, Settings{}},
		{"on", "viewer-1", SettingsChange{WatchlistByKind: &on}, Settings{WatchlistByKind: true}},
		{"a change of nothing", "viewer-1", SettingsChange{}, Settings{WatchlistByKind: true}},
		{"another person's", "viewer-2", SettingsChange{}, Settings{}},
		{"off", "viewer-1", SettingsChange{WatchlistByKind: &off}, Settings{}},
		{"on again", "viewer-1", SettingsChange{WatchlistByKind: &on}, Settings{WatchlistByKind: true}},
	} {
		got, err := st.ChangeSettings(ctx, step.user, step.change)
		if err != nil || got != step.want {
			t.Errorf("%s: %+v, %v; want %+v", step.name, got, err, step.want)
		}
		if read, err := st.GetSettings(ctx, step.user); err != nil || read != step.want {
			t.Errorf("%s, read: %+v, %v; want %+v", step.name, read, err, step.want)
		}
	}
}

// With no database nothing is kept, and the settings are the first values.
func Test16C25SettingsWithoutADatabase(t *testing.T) {
	ctx := context.Background()
	on := true
	var none *Store
	if got, err := none.ChangeSettings(ctx, "viewer-1", SettingsChange{WatchlistByKind: &on}); err != nil || got != (Settings{}) {
		t.Errorf("no store: %+v, %v", got, err)
	}
	if got, err := none.GetSettings(ctx, "viewer-1"); err != nil || got != (Settings{}) {
		t.Errorf("no store, read: %+v, %v", got, err)
	}
}
