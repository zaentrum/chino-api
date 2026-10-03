package katalog

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

// artworkUpstream is a fake katalog-manager serving one person's portrait
// with an ETag, answering a matching If-None-Match with 304. requests returns
// what it got so far.
func artworkUpstream(t *testing.T) (srv *httptest.Server, requests func() []*http.Request) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []*http.Request
	)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.Clone(r.Context()))
		mu.Unlock()
		if r.URL.Path != "/api/artwork/person/p1/profile" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", `"sha-1"`)
		w.Header().Set("Cache-Control", "public, max-age=604800")
		if r.Header.Get("If-None-Match") == `"sha-1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = io.WriteString(w, "portrait-bytes")
	}))
	t.Cleanup(srv.Close)
	return srv, func() []*http.Request {
		mu.Lock()
		defer mu.Unlock()
		return append([]*http.Request(nil), reqs...)
	}
}

// A person's portrait goes to katalog-manager with the query (the stream
// token), the bearer, Range and If-None-Match; its status, ETag and body
// come back as they are.
func TestProxyStreamServesAPersonPortraitFromTheArtworkUpstream(t *testing.T) {
	art, requests := artworkUpstream(t)
	kc := New("http://katalog-api.invalid") // must not be asked
	kc.ArtworkBaseURL = art.URL

	serve := func(path string, header http.Header) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		for k, v := range header {
			r.Header[k] = v
		}
		w := httptest.NewRecorder()
		kc.ProxyStream(w, r, "/api/artwork/person/p1/profile", "tok")
		return w
	}

	w := serve("/api/v1/people/p1/profile?stream=s.t", http.Header{"Range": {"bytes=0-3"}})
	if w.Code != http.StatusOK || w.Body.String() != "portrait-bytes" || w.Header().Get("ETag") != `"sha-1"` ||
		w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("first fetch: %d %q, headers %v", w.Code, w.Body, w.Header())
	}
	got := requests()[0]
	if got.URL.Path != "/api/artwork/person/p1/profile" || got.URL.RawQuery != "stream=s.t" ||
		got.Header.Get("Authorization") != "Bearer tok" || got.Header.Get("Range") != "bytes=0-3" {
		t.Errorf("upstream got %s %s?%s, headers %v", got.Method, got.URL.Path, got.URL.RawQuery, got.Header)
	}

	w = serve("/api/v1/people/p1/profile", http.Header{"If-None-Match": {`"sha-1"`}})
	if w.Code != http.StatusNotModified || w.Body.Len() != 0 || w.Header().Get("ETag") != `"sha-1"` {
		t.Fatalf("revalidation: %d %q, headers %v; want 304 with the ETag", w.Code, w.Body, w.Header())
	}
	if v := requests()[1].Header.Values("If-None-Match"); !reflect.DeepEqual(v, []string{`"sha-1"`}) {
		t.Errorf("upstream got If-None-Match %q", v)
	}
}

func TestProxyStreamPassesAMissingPortraitOn(t *testing.T) {
	art, _ := artworkUpstream(t)
	kc := New("http://katalog-api.invalid")
	kc.ArtworkBaseURL = art.URL
	w := httptest.NewRecorder()
	kc.ProxyStream(w, httptest.NewRequest("GET", "/api/v1/people/p2/profile", nil), "/api/artwork/person/p2/profile", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("no portrait: %d, want 404", w.Code)
	}
}
