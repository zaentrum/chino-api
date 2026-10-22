package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
)

// askAvailableRaw is POST /api/v1/me/available with body, as hdr's viewer:
// the status and the answer's fields as they came.
func askAvailableRaw(t *testing.T, h http.Handler, hdr http.Header, body string) (int, map[string]json.RawMessage) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/me/available", strings.NewReader(body))
	for k, v := range hdr {
		r.Header[k] = v
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]json.RawMessage
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("an answer that is no JSON object: %s (%v)", w.Body, err)
		}
	}
	return w.Code, out
}

// An answer that leaves out every id asked says how many titles of the
// library the viewer may watch: a phone tells the library's word - a child's
// only download above its lowered cap, a title that left - from a catalog
// imported anew, which shows nobody anything for a while. An answer with ids
// in it needs no such word, and carries none.
func TestAnAnswerThatLeavesOutEveryIDSaysHowManyTitlesTheViewerMayWatch(t *testing.T) {
	is := newIssuer(t)
	h, _, _, _ := ratedRouter(t, is)
	for _, tc := range []struct {
		name  string
		claim any
		body  string
		want  string
	}{
		// A parent lowered the cap: the child's only download is above it,
		// and the library holds what the cap allows.
		{"a child's only download above its cap", 6, `{"ids":["adult"]}`, "3"},
		// A title that left the library, for a viewer without a cap.
		{"a title that left the library", nil, `{"ids":["gone"]}`, "7"},
		// A cap that allows nothing: the library shows the viewer nothing.
		{"a cap that allows nothing", 0, `{"ids":["kid"]}`, "0"},
	} {
		code, got := askAvailableRaw(t, h, bearerWith(t, is, "viewer-1", tc.claim), tc.body)
		if code != http.StatusOK || string(got["ids"]) != "[]" || string(got["titles"]) != tc.want {
			t.Errorf("%s: %d ids %s titles %s, want [] and %s", tc.name, code, got["ids"], got["titles"], tc.want)
		}
	}
	// Some of what was asked came back: no count.
	code, got := askAvailableRaw(t, h, bearerWith(t, is, "kid-1", 6), `{"ids":["adult","kid"]}`)
	if _, there := got["titles"]; code != http.StatusOK || string(got["ids"]) != `["kid"]` || there {
		t.Errorf("an answer with ids: %d %v", code, got)
	}
	// Nothing asked, nothing answered, and nothing counted.
	code, got = askAvailableRaw(t, h, bearerWith(t, is, "viewer-1", nil), `{"ids":[]}`)
	if _, there := got["titles"]; code != http.StatusOK || there {
		t.Errorf("no ids: %d %v", code, got)
	}
}

// A catalog imported anew holds nothing yet: the empty answer says 0, and a
// phone deletes nothing on it alone. A catalog whose list cannot be had
// answers 502, as any answer the catalog cannot give.
func TestAnEmptyCatalogSaysNoTitlesAndAFailingOneNoAnswer(t *testing.T) {
	is := newIssuer(t)
	for _, tc := range []struct {
		name   string
		list   func(w http.ResponseWriter)
		status int
		titles string
	}{
		{"imported anew", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[],"total":0}`))
		}, http.StatusOK, "0"},
		{"a list that fails", func(w http.ResponseWriter) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}, http.StatusBadGateway, ""},
	} {
		katalog := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/items" {
				tc.list(w)
				return
			}
			http.NotFound(w, r)
		})
		h, err := NewRouter(config.Config{OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
			KatalogBaseURL: katalog.URL, StreamBaseURL: "http://chino-stream.invalid",
			ArtworkBaseURL: "http://katalog-manager.invalid", StreamSigningKey: signingKey}, nil, eventsse.NewBroker())
		if err != nil {
			t.Fatal(err)
		}
		code, got := askAvailableRaw(t, h, bearerWith(t, is, "viewer-1", nil), `{"ids":["film","other"]}`)
		if code != tc.status || (tc.status == http.StatusOK && (string(got["ids"]) != "[]" || string(got["titles"]) != tc.titles)) {
			t.Errorf("%s: %d %v, want %d titles %q", tc.name, code, got, tc.status, tc.titles)
		}
	}
}
