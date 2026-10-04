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
	h := adminRouter(t, is, manager.URL, katalog.URL, nil)
	admin := is.tokenWith(t, "admin-1", "chino", realm("zaentrum-admin", "zaentrum-user"))
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
		{"a viewer", "", http.Header{"Authorization": {"Bearer " + is.tokenWith(t, "viewer-1", "chino", realm("zaentrum-user"))}}, http.StatusForbidden},
		{"no credential", "", nil, http.StatusUnauthorized},
		{"a stream token", "?stream=" + streamToken(t), nil, http.StatusUnauthorized},
		{"a token of another audience", "", http.Header{"Authorization": {"Bearer " + is.tokenWith(t, "admin-1", "elsewhere", realm("zaentrum-admin"))}},
			http.StatusUnauthorized},
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
	admin := is.tokenWith(t, "admin-1", "chino", realm("zaentrum-admin"))
	h := adminRouter(t, is, "", "http://katalog-api.invalid", nil)
	if w := do(h, "POST", "/api/v1/admin/items/m1/package", http.Header{"Authorization": {"Bearer " + admin}}); w.Code != http.StatusServiceUnavailable ||
		!strings.Contains(w.Body.String(), "KATALOG_MANAGER_URL") {
		t.Errorf("no katalog-manager: %d %q, want 503 naming KATALOG_MANAGER_URL", w.Code, w.Body)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	h = adminRouter(t, is, gone.URL, "http://katalog-api.invalid", nil)
	w := do(h, "POST", "/api/v1/admin/items/m1/package?token="+admin, nil)
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), admin) {
		t.Errorf("katalog-manager gone: %d %q, want 502 without the bearer", w.Code, w.Body)
	}
}

// realm is the claim a token carries its realm roles in.
func realm(roles ...string) map[string]any {
	return map[string]any{"realm_access": map[string]any{"roles": roles}}
}

// The admin routes are for a bearer whose realm roles (realm_access.roles)
// carry the admin role, zaentrum-admin unless ADMIN_ROLE names another: not
// for a role of another name, the role as a client's role or a claim of
// another name, nor roles in another shape. ADMIN_SUBJECTS, deprecated, lets
// the subjects it lists through besides; a stream token, a subject alone, is
// never an admin's. A refusal names the role.
func TestTheAdminRoleOpensTheAdminRoutes(t *testing.T) {
	is := newIssuer(t)
	manager := newFake(t, map[string]string{"/api/items/m1/package": `{"status":"pending"}`})
	for _, tc := range []struct {
		name   string
		with   func(*config.Config)
		claims map[string]any
		sub    string
		code   int
	}{
		{"the admin role", nil, realm("zaentrum-user", "zaentrum-admin"), "u1", 200},
		{"a viewer's roles", nil, realm("zaentrum-user", "offline_access"), "u2", 403},
		{"no roles", nil, nil, "u3", 403},
		{"a role named admin", nil, realm("admin"), "u4", 403},
		{"the role as a client's", nil, map[string]any{"resource_access": map[string]any{"chino": map[string]any{"roles": []string{"zaentrum-admin"}}}}, "u5", 403},
		{"the role in another claim", nil, map[string]any{"roles": []string{"zaentrum-admin"}}, "u6", 403},
		{"the roles a string", nil, map[string]any{"realm_access": map[string]any{"roles": "zaentrum-admin"}}, "u7", 403},
		{"the roles mixed", nil, map[string]any{"realm_access": map[string]any{"roles": []any{"zaentrum-admin", 7}}}, "u8", 403},
		{"another ADMIN_ROLE, its role", func(c *config.Config) { c.AdminRole = "catalog-admin" }, realm("catalog-admin"), "u9", 200},
		{"another ADMIN_ROLE, the default role", func(c *config.Config) { c.AdminRole = "catalog-admin" }, realm("zaentrum-admin"), "u10", 403},
		{"a subject ADMIN_SUBJECTS lists", func(c *config.Config) { c.AdminSubjects = []string{"listed", "u11"} }, realm("zaentrum-user"), "u11", 200},
		{"a subject it does not list", func(c *config.Config) { c.AdminSubjects = []string{"listed"} }, realm("zaentrum-user"), "u12", 403},
		{"ADMIN_SUBJECTS set, the admin role", func(c *config.Config) { c.AdminSubjects = []string{"listed"} }, realm("zaentrum-admin"), "u13", 200},
	} {
		h := adminRouter(t, is, manager.URL, "http://katalog-api.invalid", tc.with)
		before := len(manager.requests())
		w := do(h, "POST", "/api/v1/admin/items/m1/package", http.Header{"Authorization": {"Bearer " + is.tokenWith(t, tc.sub, "chino", tc.claims)}})
		forwarded := len(manager.requests()) - before
		if w.Code != tc.code || (tc.code == 200) != (forwarded == 1) {
			t.Errorf("%s: %d %q, %d forwarded; want %d", tc.name, w.Code, w.Body, forwarded, tc.code)
		}
		if tc.code == 403 {
			role := "zaentrum-admin"
			if strings.HasPrefix(tc.name, "another ADMIN_ROLE") {
				role = "catalog-admin"
			}
			if want := "admin access required: the " + role + " role\n"; w.Body.String() != want {
				t.Errorf("%s: the refusal says %q, want %q", tc.name, w.Body, want)
			}
		}
	}

	// A stream token carries a subject alone, and the admin routes take none.
	h := adminRouter(t, is, manager.URL, "http://katalog-api.invalid", func(c *config.Config) { c.AdminSubjects = []string{"user-1"} })
	if w := do(h, "POST", "/api/v1/admin/items/m1/package?stream="+streamToken(t), nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a stream token of a listed subject: %d, want 401", w.Code)
	}
}
