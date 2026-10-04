package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
)

// adminRouter is chino-api with OIDC verified against is, katalog-manager at
// managerURL, katalog-api at katalogURL and the admin access cfg gives it.
func adminRouter(t *testing.T, is *issuer, managerURL, katalogURL string, with func(*config.Config)) http.Handler {
	t.Helper()
	cfg := config.Config{
		OIDCIssuer:        is.URL,
		OIDCAudience:      "chino",
		OIDCEnabled:       true,
		KatalogBaseURL:    katalogURL,
		KatalogManagerURL: managerURL,
		ArtworkBaseURL:    "http://katalog-manager.invalid",
		StreamBaseURL:     "http://chino-stream.invalid",
		StreamSigningKey:  signingKey,
	}
	if with != nil {
		with(&cfg)
	}
	h, err := NewRouter(cfg, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The admin packaging routes go to katalog-manager, the catalog's writer, the
// admin's bearer with them, and katalog-manager's answer comes back as it is,
// an error too; katalog-api, which only reads, is never asked. A caller who
// is no admin gets 403, one without a bearer, or with a stream token alone,
// 401, and nothing is forwarded.
func TestTheAdminPackagingRoutesGoToKatalogManager(t *testing.T) {
	is := newIssuer(t)
	manager := newFake(t, map[string]string{
		"/api/items/m1/package":       `{"status":"pending","alreadyActive":false,"message":"Packaging enqueued."}`,
		"/api/analyze/items/m1/steps": `{"itemId":"m1","steps":{"package":"pending","transcode":"done"}}`,
	})
	katalog := newFake(t, map[string]string{})
	h := adminRouter(t, is, manager.URL, katalog.URL, func(c *config.Config) { c.AdminSubjects = []string{"admin-1"} })
	admin := is.token(t, "admin-1", "chino")
	bearer := http.Header{"Authorization": {"Bearer " + admin}}

	for _, tc := range []struct {
		method, path, query string
		header              http.Header
		code                int
		body, upstream      string
	}{
		{"POST", "/api/v1/admin/items/m1/package", "", bearer, 200,
			`{"status":"pending","alreadyActive":false,"message":"Packaging enqueued."}`, "POST /api/items/m1/package"},
		{"GET", "/api/v1/admin/items/m1/package", "", bearer, 200,
			`{"itemId":"m1","steps":{"package":"pending","transcode":"done"}}`, "GET /api/analyze/items/m1/steps"},
		{"POST", "/api/v1/admin/items/gone/package", "", bearer, 404, "404 page not found\n", "POST /api/items/gone/package"},
		{"POST", "/api/v1/admin/items/m1/package", "?token=" + admin, nil, 200,
			`{"status":"pending","alreadyActive":false,"message":"Packaging enqueued."}`, "POST /api/items/m1/package"},
	} {
		w := do(h, tc.method, tc.path+tc.query, tc.header)
		if w.Code != tc.code || w.Body.String() != tc.body {
			t.Errorf("%s %s%s: %d %q, want %d %q", tc.method, tc.path, tc.query, w.Code, w.Body, tc.code, tc.body)
		}
		reqs := manager.requests()
		if len(reqs) != 1 {
			t.Fatalf("%s %s%s: katalog-manager got %d requests, want 1", tc.method, tc.path, tc.query, len(reqs))
		}
		got := reqs[0]
		if got.Method+" "+got.URL.Path != tc.upstream || got.Header.Get("Authorization") != "Bearer "+admin || got.URL.RawQuery != "" {
			t.Errorf("%s %s%s: katalog-manager got %s %s?%s with %q, want %s with the admin's bearer",
				tc.method, tc.path, tc.query, got.Method, got.URL.Path, got.URL.RawQuery, got.Header.Get("Authorization"), tc.upstream)
		}
		manager.mu.Lock()
		manager.reqs = nil
		manager.mu.Unlock()
	}

	for _, tc := range []struct {
		name   string
		path   string
		header http.Header
		code   int
	}{
		{"a viewer", "", http.Header{"Authorization": {"Bearer " + is.token(t, "viewer-1", "chino")}}, http.StatusForbidden},
		{"no credential", "", nil, http.StatusUnauthorized},
		{"a stream token", "?stream=" + streamToken(t), nil, http.StatusUnauthorized},
		{"a token of another audience", "", http.Header{"Authorization": {"Bearer " + is.token(t, "admin-1", "elsewhere")}}, http.StatusUnauthorized},
	} {
		for _, method := range []string{"POST", "GET"} {
			if w := do(h, method, "/api/v1/admin/items/m1/package"+tc.path, tc.header); w.Code != tc.code {
				t.Errorf("%s, %s: %d %q, want %d", tc.name, method, w.Code, w.Body, tc.code)
			}
		}
	}
	if n := len(manager.requests()) + len(katalog.requests()); n != 0 {
		t.Errorf("%d requests went upstream for callers refused, or to katalog-api", n)
	}
}

// Without katalog-manager the admin routes say so (503); one that does not
// answer is a 502 whose words carry no credential.
func TestTheAdminPackagingRoutesWithoutKatalogManager(t *testing.T) {
	is := newIssuer(t)
	admin := is.token(t, "admin-1", "chino")
	subjects := func(c *config.Config) { c.AdminSubjects = []string{"admin-1"} }
	h := adminRouter(t, is, "", "http://katalog-api.invalid", subjects)
	if w := do(h, "POST", "/api/v1/admin/items/m1/package", http.Header{"Authorization": {"Bearer " + admin}}); w.Code != http.StatusServiceUnavailable ||
		!strings.Contains(w.Body.String(), "KATALOG_MANAGER_URL") {
		t.Errorf("no katalog-manager: %d %q, want 503 naming KATALOG_MANAGER_URL", w.Code, w.Body)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	h = adminRouter(t, is, gone.URL, "http://katalog-api.invalid", subjects)
	w := do(h, "POST", "/api/v1/admin/items/m1/package?token="+admin, nil)
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), admin) {
		t.Errorf("katalog-manager gone: %d %q, want 502 without the bearer", w.Code, w.Body)
	}
}
