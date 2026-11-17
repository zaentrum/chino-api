package katalog

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/chino-api/internal/auth"
)

// kindCatalog is a katalog-api that answers each item after a moment: an id
// that starts with "m" a movie, with "s" a series, with "e" an episode,
// "broken" a failure and any other none. It counts the requests it gets and
// the most it had at once.
type kindCatalog struct {
	*httptest.Server
	mu     sync.Mutex
	asked  map[string]int
	now    int
	most   int
	capped int
}

func newKindCatalog(t *testing.T) *kindCatalog {
	t.Helper()
	k := &kindCatalog{asked: map[string]int{}}
	k.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/items/")
		k.mu.Lock()
		k.asked[id]++
		k.now++
		k.most = max(k.most, k.now)
		if r.URL.Query().Has("max_rating") {
			k.capped++
		}
		k.mu.Unlock()
		defer func() {
			k.mu.Lock()
			k.now--
			k.mu.Unlock()
		}()
		time.Sleep(10 * time.Millisecond)
		typ := map[byte]string{'m': "movie", 's': "series", 'e': "episode"}[id[0]]
		switch {
		case id == "broken":
			http.Error(w, "the catalog is down", http.StatusInternalServerError)
		case typ == "":
			http.NotFound(w, r)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"`+id+`","type":"`+typ+`","title":"An invented title"}`)
		}
	}))
	t.Cleanup(k.Close)
	return k
}

// take is how many requests each id got since the last take, and the most
// that were out at once.
func (k *kindCatalog) take() (asked map[string]int, most int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	asked, most = k.asked, k.most
	k.asked, k.most = map[string]int{}, 0
	return asked, most
}

// Kinds asks katalog-api each id once, at most kindRequests at a time, keeps
// each answer (an id of no title too), and asks a capped viewer's anew, with
// the cap; a failure is an error, and is not kept.
func Test8C18KindsAreAskedAFewAtATimeAndKept(t *testing.T) {
	k := newKindCatalog(t)
	c := New(k.URL)
	ctx := context.Background()
	var ids []string
	want := map[string]string{"gone": ""}
	for i := range 20 {
		ids = append(ids, fmt.Sprintf("m%d", i))
		want[fmt.Sprintf("m%d", i)] = "movie"
	}
	for i := range 6 {
		ids = append(ids, fmt.Sprintf("s%d", i), fmt.Sprintf("e%d", i))
		want[fmt.Sprintf("s%d", i)], want[fmt.Sprintf("e%d", i)] = "series", "episode"
	}
	ids = append(ids, "gone", "m0")

	got, err := c.Kinds(ctx, "a-bearer", ids)
	if err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("kinds: %v, %v; want %v", got, err, want)
	}
	asked, most := k.take()
	if len(asked) != len(want) {
		t.Errorf("%d ids asked, want %d", len(asked), len(want))
	}
	for id, n := range asked {
		if n != 1 {
			t.Errorf("%s asked %d times", id, n)
		}
	}
	if most > kindRequests || most < 2 {
		t.Errorf("%d requests at once, want 2 to %d", most, kindRequests)
	}

	// Kept: asked again, nothing goes to katalog-api.
	if got, err := c.Kinds(ctx, "a-bearer", ids); err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("kept: %v, %v", got, err)
	}
	if asked, _ := k.take(); len(asked) != 0 {
		t.Errorf("kept, yet asked %v", asked)
	}

	// A capped viewer's are asked anew, with the cap.
	capped := auth.WithMaxRating(ctx, 12)
	if got, err := c.Kinds(capped, "a-bearer", []string{"m0", "s0"}); err != nil || got["m0"] != "movie" || got["s0"] != "series" {
		t.Errorf("capped: %v, %v", got, err)
	}
	if asked, _ := k.take(); len(asked) != 2 || k.capped != 2 {
		t.Errorf("capped: asked %v, %d with the cap", asked, k.capped)
	}

	// A failure is the error, and is asked again the next time.
	for range 2 {
		if got, err := c.Kinds(ctx, "a-bearer", []string{"m0", "broken"}); err == nil {
			t.Errorf("a failing catalog: %v, no error", got)
		}
		if asked, _ := k.take(); asked["broken"] != 1 || asked["m0"] != 0 {
			t.Errorf("a failing catalog: asked %v", asked)
		}
	}
}
