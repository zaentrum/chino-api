package eventsse

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// The bridge tails the item topics as before, and apart from them the topic of
// a title's packaged extras, each under the tenant prefix (with its dot or
// without; stube. when there is none).
func TestTheExtrasTopicIsTailedApart(t *testing.T) {
	items := func(p string) []string {
		return []string{p + "catalog.item.discovered", p + "catalog.item.enriched", p + "catalog.item.analyzed",
			p + "catalog.item.transcoded", p + "catalog.item.packaged", p + "catalog.item.removed"}
	}
	for prefix, p := range map[string]string{"": "stube.", "tenant-a": "tenant-a.", "tenant-a.": "tenant-a.", " stube. ": "stube."} {
		if got := catalogTopics(prefix); !reflect.DeepEqual(got, items(p)) {
			t.Errorf("prefix %q: item topics %v, want %v", prefix, got, items(p))
		}
		if got := extraTopic(prefix); got != p+"catalog.extra.packaged" {
			t.Errorf("prefix %q: extras topic %q", prefix, got)
		}
	}
}

// The extras' tail starts once its topic exists: asked again every interval
// while it is not there (or the cluster does not answer), and not at all
// once the bridge stops.
func TestWaitForTopic(t *testing.T) {
	answers := []error{errors.New("no broker answered"), nil, nil}
	asked := 0
	exists := func(context.Context) (bool, error) {
		asked++
		if asked <= len(answers) {
			return false, answers[asked-1]
		}
		return true, nil
	}
	if !waitForTopic(context.Background(), time.Millisecond, "stube.catalog.extra.packaged", exists) || asked != 4 {
		t.Errorf("asked %d times, want the topic found on the 4th", asked)
	}

	ctx, cancel := context.WithCancel(context.Background())
	asked = 0
	never := func(context.Context) (bool, error) {
		asked++
		if asked == 2 {
			cancel()
		}
		return false, nil
	}
	done := make(chan bool)
	go func() { done <- waitForTopic(ctx, time.Millisecond, "stube.catalog.extra.packaged", never) }()
	select {
	case found := <-done:
		if found {
			t.Error("found a topic that never came")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting after the bridge stopped")
	}
}

// A message is a note for the title it names. An item event's phase is its
// topic's step; a packaged extra's is "extra.packaged", not "packaged", which
// would say the title itself became watchable. What is no envelope is no
// note.
func TestNoteOf(t *testing.T) {
	for _, tc := range []struct {
		topic, value string
		want         Note
		ok           bool
	}{
		{"stube.catalog.item.packaged", `{"eventId":"e1","itemId":"m1","type":"movie","step":"package","status":"done"}`,
			Note{ItemID: "m1", ItemType: "movie", Phase: "packaged"}, true},
		{"stube.catalog.extra.packaged", `{"eventId":"e2","itemId":"m1","type":"movie","step":"extra","status":"done",
			"extraId":"1b5c2a8e-6f0d-4c3e-9a51-2d7f0c4b8e01","kind":"trailer","occurredAt":"2026-10-06T08:00:00Z","source":"katalog-manager"}`,
			Note{ItemID: "m1", ItemType: "movie", Phase: "extra.packaged"}, true},
		{"tenant-a.catalog.extra.packaged", `{"itemId":"s1","type":"series","step":"extra","status":"done"}`,
			Note{ItemID: "s1", ItemType: "series", Phase: "extra.packaged"}, true},
		{"stube.catalog.item.removed", `{"itemId":"m1"}`, Note{ItemID: "m1", Phase: "removed"}, true},
		{"stube.catalog.extra.packaged", `not an envelope`, Note{}, false},
	} {
		got, ok := noteOf(tc.topic, []byte(tc.value))
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s %s: %+v %v, want %+v %v", tc.topic, tc.value, got, ok, tc.want, tc.ok)
		}
	}
}
