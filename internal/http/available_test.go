package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
)

// askAvailable is POST /api/v1/me/available with body, as hdr's viewer: the
// status and the ids answered.
func askAvailable(t *testing.T, h http.Handler, hdr http.Header, body string) (int, []string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/me/available", strings.NewReader(body))
	for k, v := range hdr {
		r.Header[k] = v
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out struct {
		IDs []string `json:"ids"`
	}
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.IDs == nil {
			t.Fatalf("an answer that is no list of ids: %s (%v)", w.Body, err)
		}
	}
	return w.Code, out.IDs
}

// The phone asks, in one request, which of its downloads the viewer may still
// watch, and deletes the others. A capped viewer's cap decides, as on every
// route of one title: a title above it, an unrated one and an id of no title
// are left out. For a viewer without a cap the catalog decides: a title it
// holds. The answer keeps the order asked, each id once.
func TestAvailableIsWhatTheViewerMayWatchNow(t *testing.T) {
	is := newIssuer(t)
	h, _, _, _ := ratedRouter(t, is)
	asked := `{"ids":["adult","kid"," kid ","unrated","gone","ep16","ep6",""]}`
	for _, tc := range []struct {
		name  string
		claim any
		want  []string
	}{
		{"a viewer without a cap", nil, []string{"adult", "kid", "unrated", "ep16", "ep6"}},
		{"a viewer capped at 12", 12, []string{"kid", "ep6"}},
		{"a viewer capped at 18", 18, []string{"adult", "kid", "ep16", "ep6"}},
		{"a viewer capped at 0", 0, []string{}},
	} {
		code, got := askAvailable(t, h, bearerWith(t, is, "viewer-1", tc.claim), asked)
		if code != http.StatusOK || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %d %v, want %v", tc.name, code, got, tc.want)
		}
	}

	// A parent lowers a child's cap: what the child downloaded above the new
	// cap is no longer the child's to watch.
	before := bearerWith(t, is, "kid-1", 16)
	after := bearerWith(t, is, "kid-1", 6)
	if _, got := askAvailable(t, h, before, `{"ids":["adult","kid"]}`); !reflect.DeepEqual(got, []string{"adult", "kid"}) {
		t.Errorf("at 16: %v", got)
	}
	if _, got := askAvailable(t, h, after, `{"ids":["adult","kid"]}`); !reflect.DeepEqual(got, []string{"kid"}) {
		t.Errorf("at 6: %v", got)
	}

	// Nothing asked, nothing answered.
	if code, got := askAvailable(t, h, bearerWith(t, is, "viewer-1", nil), `{"ids":[]}`); code != http.StatusOK || len(got) != 0 {
		t.Errorf("no ids: %d %v", code, got)
	}
}

// What is no question is refused, and nobody signed in is not answered.
func TestAvailableTakesASignedInViewersListOfIDs(t *testing.T) {
	is := newIssuer(t)
	h, _, _, _ := ratedRouter(t, is)
	viewer := bearerWith(t, is, "viewer-1", nil)
	many := make([]string, maxAvailableIDs+1)
	for i := range many {
		many[i] = fmt.Sprintf(`"t%d"`, i)
	}
	for _, tc := range []struct {
		name string
		hdr  http.Header
		body string
		want int
	}{
		{"no bearer", nil, `{"ids":["kid"]}`, http.StatusUnauthorized},
		{"no body", viewer, ``, http.StatusBadRequest},
		{"no JSON", viewer, `ids=kid`, http.StatusBadRequest},
		{"too many ids", viewer, `{"ids":[` + strings.Join(many, ",") + `]}`, http.StatusBadRequest},
	} {
		if code, _ := askAvailable(t, h, tc.hdr, tc.body); code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, code, tc.want)
		}
	}
}

// When the catalog cannot say what a viewer may watch, the answer is 502 and
// no ids: a phone deletes nothing for want of an answer.
func TestAvailableSaysNothingWhenTheCatalogCannot(t *testing.T) {
	is := newIssuer(t)
	broken := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/visible":
			http.Error(w, "boom", http.StatusInternalServerError)
		case r.URL.Path == "/api/v1/items/broken":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/items/")
			_, _ = fmt.Fprintf(w, `{"id":%q,"type":"movie","title":%q}`, id, id)
		}
	})
	h, err := NewRouter(config.Config{OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: broken.URL, StreamBaseURL: "http://chino-stream.invalid",
		ArtworkBaseURL: "http://katalog-manager.invalid", StreamSigningKey: signingKey}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	if code, got := askAvailable(t, h, bearerWith(t, is, "viewer-1", nil), `{"ids":["a","broken","b"]}`); code != http.StatusBadGateway || got != nil {
		t.Errorf("an uncapped viewer, a lookup failing: %d %v", code, got)
	}
	if code, got := askAvailable(t, h, bearerWith(t, is, "kid-1", 12), `{"ids":["a"]}`); code != http.StatusBadGateway || got != nil {
		t.Errorf("a capped viewer, /visible failing: %d %v", code, got)
	}
	if code, got := askAvailable(t, h, bearerWith(t, is, "viewer-1", nil), `{"ids":["a","b"]}`); code != http.StatusOK || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("the catalog answering: %d %v", code, got)
	}
}
