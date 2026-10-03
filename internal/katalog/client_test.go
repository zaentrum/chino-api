package katalog

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// upstream is a fake katalog-api (or katalog-manager): it answers each path
// in routes with the JSON given and records the requests it gets.
type upstream struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
}

func newUpstream(t *testing.T, routes map[string]string) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.reqs = append(u.reqs, r.Clone(context.Background()))
		u.mu.Unlock()
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(u.Close)
	return u
}

// last is the last request the upstream got.
func (u *upstream) last(t *testing.T) *http.Request {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		t.Fatal("the upstream got no request")
	}
	return u.reqs[len(u.reqs)-1]
}

// sameJSON fails t unless got marshals to the JSON want holds.
func sameJSON(t *testing.T, what string, got any, want string) {
	t.Helper()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var g, w any
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("%s: the expected JSON: %v", what, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s:\n got %s\nwant %s", what, raw, want)
	}
}

// A title's cast comes through as katalog-api sends it: every entry, in its
// order, with every field — no trim to eight, no reordering.
func TestItemDetailPassesTheCastThrough(t *testing.T) {
	var entries []string
	for i, c := range "ABCDEFGHIJKL" { // twelve actors, billed 0..11
		entries = append(entries, `{"person_id":"actor-`+string(c)+`","name":"Actor `+string(c)+
			`","role":"actor","character":"Part `+string(c)+`","order":`+itoa(i)+`}`)
	}
	entries = append(entries,
		`{"person_id":"creator-a","name":"Creator A","role":"creator","episode_count":12}`,
		`{"person_id":"director-a","name":"Director A","role":"director","job":"Director"}`,
		`{"person_id":"writer-b","name":"Writer B","role":"writer","job":"Screenplay","order":0}`,
		`{"person_id":"stunt-a","name":"Stunt A","role":"stunts"}`,
		`{"name":"Unlinked A","role":"actor"}`, // katalog-api's order, even where it is not role order
	)
	cast := "[" + strings.Join(entries, ",") + "]"
	up := newUpstream(t, map[string]string{
		"/api/v1/items/m1": `{"id":"m1","type":"movie","title":"A Film","cast":` + cast + `}`,
	})
	kc := New(up.URL)

	it, err := kc.GetItemDetail(context.Background(), "tok", "m1")
	if err != nil || it == nil {
		t.Fatalf("item detail: %v, %v", it, err)
	}
	sameJSON(t, "cast", it.Cast, cast)
	if q := up.last(t).URL.Query().Get("include"); !strings.Contains(q, "cast") {
		t.Errorf("include %q, want the cast asked for", q)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
