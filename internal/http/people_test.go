package http

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
)

// fake is an upstream answering each path in routes with the body given (JSON
// unless it says otherwise) and recording the requests it gets.
type fake struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
}

func newFake(t *testing.T, routes map[string]string) *fake {
	t.Helper()
	f := &fake{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Clone(r.Context()))
		f.mu.Unlock()
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if len(body) > 0 && (body[0] == '{' || body[0] == '[') {
			w.Header().Set("Content-Type", "application/json")
		} else {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("ETag", `"sha-1"`)
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fake) requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.reqs...)
}

const signingKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 bytes, base64

// router is chino-api with katalog-api at katalogURL and katalog-manager at
// artworkURL. With oidc, its issuer is one that is never asked: each request
// carries a stream token or no credential at all.
func router(t *testing.T, katalogURL, artworkURL string, oidc bool) http.Handler {
	t.Helper()
	h, err := NewRouter(config.Config{
		OIDCIssuer:       "http://127.0.0.1:1/realms/none",
		OIDCAudience:     "chino-web",
		OIDCEnabled:      oidc,
		KatalogBaseURL:   katalogURL,
		ArtworkBaseURL:   artworkURL,
		StreamBaseURL:    "http://chino-stream.invalid",
		StreamSigningKey: signingKey,
	}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// streamToken is a stream token for user-1, as POST /me/stream-token mints.
func streamToken(t *testing.T) string {
	t.Helper()
	s, err := auth.NewSigner(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := s.Mint("user-1", time.Hour)
	return tok
}

func do(h http.Handler, method, path string, header http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	for k, v := range header {
		r.Header[k] = v
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// /api/v1/people/{id}/profile is the portrait from katalog-manager, behind
// the posters' auth: a stream token is enough (an <img src> cannot send a
// bearer), and it rides on to katalog-manager with the query.
func TestPersonProfileIsProxiedWithThePostersAuth(t *testing.T) {
	art := newFake(t, map[string]string{"/api/artwork/person/p1/profile": "portrait-bytes"})
	kat := newFake(t, map[string]string{"/api/v1/people/p1": `{"id":"p1","name":"Ada","has_profile":true,"items":[]}`})
	h := router(t, kat.URL, art.URL, true)
	tok := streamToken(t)

	w := do(h, "GET", "/api/v1/people/p1/profile?stream="+tok, nil)
	if w.Code != http.StatusOK || w.Body.String() != "portrait-bytes" || w.Header().Get("ETag") != `"sha-1"` {
		t.Fatalf("with a stream token: %d %q, headers %v", w.Code, w.Body, w.Header())
	}
	reqs := art.requests()
	if len(reqs) != 1 || reqs[0].URL.Path != "/api/artwork/person/p1/profile" || reqs[0].URL.Query().Get("stream") != tok {
		t.Fatalf("katalog-manager got %v", reqs)
	}

	if w = do(h, "GET", "/api/v1/people/p1/profile", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("without a credential: %d, want 401", w.Code)
	}
	// The stream token opens the media routes only, not the person itself.
	if w = do(h, "GET", "/api/v1/people/p1?stream="+tok, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("the person with a stream token: %d, want 401", w.Code)
	}
	if n := len(art.requests()) + len(kat.requests()); n != 1 {
		t.Errorf("%d upstream requests, want only the one portrait", n)
	}
}
