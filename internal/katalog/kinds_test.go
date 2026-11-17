package katalog

import (
	"context"
	"errors"
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

// kindCatalog is a katalog-api that answers each item after its delay (10 ms
// at first): an id that starts with "m" a movie, with "s" a series, with "e"
// an episode, with "broken" a failure, with "slow" nothing until the request
// is given up, and any other none. While gate is set, each request waits for
// it to close. It counts the requests it gets and the most it had at once.
type kindCatalog struct {
	*httptest.Server
	mu     sync.Mutex
	asked  map[string]int
	now    int
	most   int
	capped int
	delay  time.Duration
	gate   chan struct{}
}

func newKindCatalog(t *testing.T) *kindCatalog {
	t.Helper()
	k := &kindCatalog{asked: map[string]int{}, delay: 10 * time.Millisecond}
	k.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/items/")
		k.mu.Lock()
		k.asked[id]++
		k.now++
		k.most = max(k.most, k.now)
		if r.URL.Query().Has("max_rating") {
			k.capped++
		}
		delay, gate := k.delay, k.gate
		k.mu.Unlock()
		defer func() {
			k.mu.Lock()
			k.now--
			k.mu.Unlock()
		}()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		if strings.HasPrefix(id, "slow") {
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		}
		time.Sleep(delay)
		typ := map[byte]string{'m': "movie", 's': "series", 'e': "episode"}[id[0]]
		switch {
		case strings.HasPrefix(id, "broken"):
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

// set changes how k answers, between requests.
func (k *kindCatalog) set(delay time.Duration, gate chan struct{}) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.delay, k.gate = delay, gate
}

// asking is how many requests for a title's kind c has out.
func asking(c *Client) int {
	c.visible.mu.Lock()
	defer c.visible.mu.Unlock()
	return len(c.visible.kindAsks)
}

// out is how many requests are out at k now.
func (k *kindCatalog) out() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.now
}

// waitOut waits until n requests are out at k.
func (k *kindCatalog) waitOut(t *testing.T, n int) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); k.out() < n; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("%d requests out, never %d", k.out(), n)
		}
	}
}

// ids is n ids that start with prefix.
func ids(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

// Kinds asks katalog-api each id once, at most kindRequests at a time, keeps
// each answer (an id of no title too), and asks a capped viewer's anew, with
// the cap.
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
}

// Calls at once for the same titles ask katalog-api once per title: the
// later ones wait for the answers the first asked for, and each gets every
// kind.
func Test8C18KindsOfTheSameTitlesAtOnceAreAskedOnce(t *testing.T) {
	k := newKindCatalog(t)
	gate := make(chan struct{})
	k.set(10*time.Millisecond, gate)
	c := New(k.URL)
	titles := append(ids("m", 20), ids("s", 20)...)

	const calls = 3
	answers := make([]map[string]string, calls)
	errs := make([]error, calls)
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i], errs[i] = c.Kinds(context.Background(), "a-bearer", titles)
		}()
		if i == 0 {
			k.waitOut(t, kindRequests) // the first call's requests are out
		}
	}
	time.Sleep(50 * time.Millisecond) // the others have asked for the same titles
	k.set(10*time.Millisecond, nil)
	close(gate)
	wg.Wait()

	for i := range calls {
		if errs[i] != nil || len(answers[i]) != len(titles) {
			t.Errorf("call %d: %d kinds, %v; want %d", i, len(answers[i]), errs[i], len(titles))
		}
		for _, id := range titles {
			if want := map[byte]string{'m': "movie", 's': "series"}[id[0]]; answers[i][id] != want {
				t.Errorf("call %d: %s is %q, want %q", i, id, answers[i][id], want)
			}
		}
	}
	asked, most := k.take()
	if len(asked) != len(titles) || most > kindRequests {
		t.Errorf("%d titles asked, %d at once; want %d, at most %d", len(asked), most, len(titles), kindRequests)
	}
	for id, n := range asked {
		if n != 1 {
			t.Errorf("%s asked %d times, want once", id, n)
		}
	}
}

// One call asks katalog-api about maxKindAsks titles at most: the others are
// left out of its answer, and asked by the next call, which does not ask the
// ones kept.
func Test8C18OneCallAsksAboutMaxKindAsksTitles(t *testing.T) {
	k := newKindCatalog(t)
	k.set(0, nil)
	c := New(k.URL)
	titles := ids("m", maxKindAsks+1)

	got, err := c.Kinds(context.Background(), "a-bearer", titles)
	if err != nil || len(got) != maxKindAsks {
		t.Fatalf("%d kinds, %v; want %d", len(got), err, maxKindAsks)
	}
	if _, there := got[titles[maxKindAsks]]; there {
		t.Errorf("%s, past the bound, answered", titles[maxKindAsks])
	}
	if asked, _ := k.take(); len(asked) != maxKindAsks || asked[titles[maxKindAsks]] != 0 {
		t.Errorf("%d titles asked (the last %d times), want %d", len(asked), asked[titles[maxKindAsks]], maxKindAsks)
	}

	got, err = c.Kinds(context.Background(), "a-bearer", titles)
	if err != nil || len(got) != len(titles) || got[titles[maxKindAsks]] != "movie" {
		t.Errorf("the next call: %d kinds, %v; want %d", len(got), err, len(titles))
	}
	if asked, _ := k.take(); len(asked) != 1 || asked[titles[maxKindAsks]] != 1 {
		t.Errorf("the next call asked %v, want %s alone", asked, titles[maxKindAsks])
	}
}

// A slow catalog holds a viewer's answer no longer than kindWait, and a slot
// no longer than kindAskWait: another viewer's titles are answered while the
// first viewer's requests hang.
func Test8C18ASlowCatalogHoldsNobodyLong(t *testing.T) {
	t.Run("one request", func(t *testing.T) {
		k := newKindCatalog(t)
		c := New(k.URL)
		c.kindAskWait, c.kindWait = 100*time.Millisecond, 3*time.Second

		first := make(chan error, 1)
		go func() {
			_, err := c.Kinds(context.Background(), "a-bearer", ids("slow", 2*kindRequests))
			first <- err
		}()
		k.waitOut(t, kindRequests) // every slot holds a request that hangs

		start := time.Now()
		got, err := c.Kinds(context.Background(), "another-bearer", []string{"m1", "m2", "s1", "e1"})
		took := time.Since(start)
		if err != nil || got["m1"] != "movie" || got["m2"] != "movie" || got["s1"] != "series" || got["e1"] != "episode" {
			t.Errorf("the other viewer's: %v, %v", got, err)
		}
		if took > time.Second {
			t.Errorf("the other viewer's took %v, want well under a second", took)
		}
		if err := <-first; err == nil {
			t.Error("the first viewer's: no error, though katalog-api said nothing")
		}
	})

	t.Run("one call", func(t *testing.T) {
		k := newKindCatalog(t)
		c := New(k.URL)
		c.kindWait = 200 * time.Millisecond

		start := time.Now()
		got, err := c.Kinds(context.Background(), "a-bearer", []string{"slow1", "m1", "slow2"})
		took := time.Since(start)
		if err != nil || len(got) != 1 || got["m1"] != "movie" {
			t.Errorf("kinds: %v, %v; want m1 alone", got, err)
		}
		if took > time.Second {
			t.Errorf("took %v, want about 200 ms", took)
		}
		// Its requests are given up at the end of its wait, not of
		// kindAskWait's; not kept, the next call asks slow1 again, and not m1.
		for asking(c) > 0 {
			if time.Since(start) > time.Second {
				t.Fatalf("the call's requests still out %v after it began", time.Since(start))
			}
			time.Sleep(time.Millisecond)
		}
		k.take()
		if got, _ := c.Kinds(context.Background(), "a-bearer", []string{"slow1", "m1"}); len(got) != 1 {
			t.Errorf("again: %v", got)
		}
		if asked, _ := k.take(); asked["slow1"] != 1 || asked["m1"] != 0 {
			t.Errorf("again: asked %v, want slow1 alone", asked)
		}
	})
}

// A title katalog-api fails for is left out of the answer, which has the
// others, and is not kept; the answer is an error only when katalog-api said
// none of the kinds, and a call stops asking once katalog-api has failed
// kindRequests times.
func Test8C18ATitleTheCatalogCannotSayIsLeftOut(t *testing.T) {
	k := newKindCatalog(t)
	c := New(k.URL)
	ctx := context.Background()

	for range 2 {
		got, err := c.Kinds(ctx, "a-bearer", []string{"m1", "broken1", "s1", "e1"})
		if err != nil || fmt.Sprint(got) != "map[e1:episode m1:movie s1:series]" {
			t.Errorf("one failing among several: %v, %v", got, err)
		}
		if asked, _ := k.take(); asked["broken1"] != 1 {
			t.Errorf("one failing among several: asked %v, want broken1 again", asked)
		}
	}

	if got, err := c.Kinds(ctx, "a-bearer", []string{"broken1", "broken2"}); err == nil || got != nil {
		t.Errorf("all failing: %v, %v; want an error", got, err)
	}
	k.take()
	if got, err := c.Kinds(ctx, "a-bearer", ids("broken", 5*kindRequests)); err == nil || got != nil {
		t.Errorf("all failing, many: %v, %v; want an error", got, err)
	}
	if asked, _ := k.take(); len(asked) >= 3*kindRequests {
		t.Errorf("all failing, many: %d asked, want the call to stop", len(asked))
	}
}

// The titles katalog-api failed for are asked after the others the next
// time: the movies behind more failing titles than a call may fail for, none
// of which the first call says, are said by the next (by the n/kindRequests+1th
// behind n), and a failing title is never answered.
func Test8C18TheTitlesTheCatalogFailsForAreAskedLast(t *testing.T) {
	movies := ids("m", 20)
	for _, n := range []int{2*kindRequests - 1, 30} {
		t.Run(fmt.Sprintf("%d failing", n), func(t *testing.T) {
			k := newKindCatalog(t)
			c := New(k.URL)
			titles := append(ids("broken", n), movies...)
			if got, err := c.Kinds(context.Background(), "a-bearer", titles); err == nil {
				t.Fatalf("the first call: %v; want none said", got)
			}
			for call := 2; ; call++ {
				got, err := c.Kinds(context.Background(), "a-bearer", titles)
				said := 0
				for _, id := range movies {
					if got[id] == "movie" {
						said++
					}
				}
				for _, id := range ids("broken", n) {
					if kind, there := got[id]; there {
						t.Errorf("call %d: %s, which fails, answered %q", call, id, kind)
					}
				}
				if said == len(movies) && err == nil {
					break
				}
				if call == n/kindRequests+1 {
					t.Fatalf("call %d: %d of %d movies said, %v", call, said, len(movies), err)
				}
			}
		})
	}
}

// The requests of a call whose viewer goes are asked on for whoever waits for
// them: the waiter gets every kind, whether the viewer went while the requests
// were out or before they had a slot, and katalog-api is asked each title once.
func Test8C18AWaiterGetsItsKindsWhenTheAskerGoes(t *testing.T) {
	// goes has a viewer ask for titles, all movies, and go once another call
	// waits for them; then katalog-api is let answer (release), and the other
	// call is to get every kind, each title asked once.
	goes := func(t *testing.T, k *kindCatalog, c *Client, titles []string, release func()) {
		t.Helper()
		asker, leave := context.WithCancel(context.Background())
		went := make(chan error, 1)
		go func() {
			_, err := c.Kinds(asker, "a-bearer", titles)
			went <- err
		}()
		time.Sleep(50 * time.Millisecond) // the viewer has asked
		var got map[string]string
		var err error
		waited := make(chan struct{})
		go func() {
			defer close(waited)
			got, err = c.Kinds(context.Background(), "another-bearer", titles)
		}()
		time.Sleep(50 * time.Millisecond) // the other call waits for the viewer's requests
		leave()
		if err := <-went; !errors.Is(err, context.Canceled) {
			t.Errorf("the viewer who went: %v, want context canceled", err)
		}
		time.Sleep(50 * time.Millisecond) // the viewer's requests have seen it go
		release()
		<-waited
		if err != nil || len(got) != len(titles) {
			t.Errorf("the waiter: %d kinds, %v; want %d", len(got), err, len(titles))
		}
		asked, _ := k.take()
		for _, id := range titles {
			if got[id] != "movie" || asked[id] != 1 {
				t.Errorf("the waiter: %s is %q, asked %d times; want movie, once", id, got[id], asked[id])
			}
		}
	}

	t.Run("while its requests are out", func(t *testing.T) {
		k := newKindCatalog(t)
		gate := make(chan struct{})
		k.set(10*time.Millisecond, gate)
		c := New(k.URL)
		goes(t, k, c, ids("m", 5*kindRequests), func() {
			k.set(10*time.Millisecond, nil)
			close(gate)
		})
	})

	t.Run("before they had a slot", func(t *testing.T) {
		k := newKindCatalog(t)
		c := New(k.URL)
		c.kindAskWait = 300 * time.Millisecond
		slow := make(chan error, 1)
		go func() {
			_, err := c.Kinds(context.Background(), "a-bearer", ids("slow", kindRequests))
			slow <- err
		}()
		k.waitOut(t, kindRequests) // every slot holds a request that hangs, for kindAskWait
		goes(t, k, c, ids("m", 5), func() {})
		<-slow
	})
}

// That katalog-api serves no title of an id is kept for kindNoneTTL, a kind
// for kindTTL; once the first has gone, the id is asked again.
func Test8C18AnIDOfNoTitleIsKeptAMinute(t *testing.T) {
	k := newKindCatalog(t)
	c := New(k.URL)
	ctx := context.Background()
	start := time.Now()
	if got, err := c.Kinds(ctx, "a-bearer", []string{"gone", "m1"}); err != nil || fmt.Sprint(got) != "map[gone: m1:movie]" {
		t.Fatalf("kinds: %v, %v", got, err)
	}
	c.visible.mu.Lock()
	gone, movie := c.visible.kind["-|gone"], c.visible.kind["-|m1"]
	c.visible.mu.Unlock()
	if gone.until.Before(start) || gone.until.After(time.Now().Add(kindNoneTTL)) {
		t.Errorf("an id of no title kept until %v, want at most %v from now", gone.until, kindNoneTTL)
	}
	if movie.until.Before(start.Add(kindTTL)) {
		t.Errorf("a movie kept until %v, want %v from now", movie.until, kindTTL)
	}

	c.visible.mu.Lock()
	c.visible.kind["-|gone"] = answer{until: start}
	c.visible.mu.Unlock()
	k.take()
	if got, err := c.Kinds(ctx, "a-bearer", []string{"gone", "m1"}); err != nil || len(got) != 2 {
		t.Errorf("again: %v, %v", got, err)
	}
	if asked, _ := k.take(); asked["gone"] != 1 || asked["m1"] != 0 {
		t.Errorf("again: asked %v, want gone alone", asked)
	}
}

// What is kept of one kind of answer is bounded: once full, the answers that
// expire first go, and the others stay.
func Test8C18TheCacheDropsTheOldestAtItsLimit(t *testing.T) {
	now := time.Now()
	m := map[string]answer{}
	for i := range maxAnswers {
		m[fmt.Sprintf("k%d", i)] = answer{item: "movie", until: now.Add(time.Hour + time.Duration(i)*time.Millisecond)}
	}
	keep(m, "new", answer{item: "series", until: now.Add(2 * time.Hour)}, now)
	if len(m) > maxAnswers || len(m) < maxAnswers*8/10 {
		t.Fatalf("%d kept, want between %d and %d", len(m), maxAnswers*8/10, maxAnswers)
	}
	if m["new"].item != "series" {
		t.Error("the new answer is not kept")
	}
	first := -1 // the oldest that stayed
	for i := range maxAnswers {
		_, there := m[fmt.Sprintf("k%d", i)]
		if there && first < 0 {
			first = i
		}
		if !there && first >= 0 {
			t.Fatalf("k%d dropped, though k%d, older, stayed", i, first)
		}
	}
	if first <= 0 {
		t.Errorf("the oldest that stayed is k%d, want some dropped and the newest kept", first)
	}
}
