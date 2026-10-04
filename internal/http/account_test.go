package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
	"github.com/zaentrum/chino-api/internal/portal"
	"github.com/zaentrum/chino-api/internal/store"
)

const deletionToken = "account-deletion-token-for-tests-0123456789"

// fakePortal is portal-api's DELETE /api/portal/me: it records each call and
// answers status, with body.
type fakePortal struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	body   string
	calls  []*http.Request
}

func newFakePortal(t *testing.T) *fakePortal {
	t.Helper()
	f := &fakePortal{status: http.StatusNoContent}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Clone(r.Context()))
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakePortal) take() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

// accountRouter is chino-api with OIDC verified against is, portal-api at
// portalURL, the account deletion token token, and the store st.
func accountRouter(t *testing.T, is *issuer, portalURL, token string, st *store.Store) http.Handler {
	t.Helper()
	h, err := NewRouter(config.Config{
		OIDCIssuer:           is.URL,
		OIDCAudience:         "chino",
		OIDCEnabled:          true,
		KatalogBaseURL:       "http://katalog-api.invalid",
		ArtworkBaseURL:       "http://katalog-manager.invalid",
		StreamBaseURL:        "http://chino-stream.invalid",
		StreamSigningKey:     signingKey,
		PortalBaseURL:        portalURL,
		AccountDeletionToken: token,
	}, st, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func answer(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("not JSON: %s", w.Body)
	}
	return v
}

// DELETE /api/v1/me deletes the person's data and asks portal-api, with their
// bearer and the deletion token, to delete their account; what portal-api
// answers decides what the person is told. Without a bearer in the header, or
// with a stream token, nothing is asked.
func TestDeletingYourOwnAccount(t *testing.T) {
	is := newIssuer(t)
	fp := newFakePortal(t)
	h := accountRouter(t, is, fp.URL, deletionToken, nil)
	kid := is.tokenWith(t, "kid-1", "chino", realm("zaentrum-user"))
	bearer := http.Header{"Authorization": {"Bearer " + kid}}

	w := do(h, http.MethodDelete, "/api/v1/me", bearer)
	if w.Code != http.StatusOK || answer(t, w)["account"] != "deleted" {
		t.Fatalf("deleted: %d %s", w.Code, w.Body)
	}
	calls := fp.take()
	if len(calls) != 1 {
		t.Fatalf("%d calls to portal-api", len(calls))
	}
	c := calls[0]
	if c.Method != http.MethodDelete || c.URL.Path != "/api/portal/me" || c.Header.Get("Authorization") != "Bearer "+kid ||
		c.Header.Get(portal.DeletionHeader) != deletionToken {
		t.Errorf("portal-api was asked %s %s with %v", c.Method, c.URL, c.Header)
	}

	for _, tc := range []struct {
		name        string
		status      int
		body        string
		code        int
		key, want   string
		mustNotLeak string
	}{
		{"gone already", http.StatusNotFound, `{"account":"gone"}`, http.StatusOK, "account", "gone", ""},
		{"the last admin", http.StatusConflict, "You are the last admin: make someone else an admin first.\n", http.StatusConflict,
			"message", "You are the last admin: make someone else an admin first.", ""},
		{"portal-api failing", http.StatusInternalServerError, "keycloak: at java.lang.Thread", http.StatusBadGateway, "error", "account_not_deleted", "java"},
		{"portal-api refusing the token", http.StatusForbidden, "forbidden", http.StatusBadGateway, "error", "account_not_deleted", ""},
	} {
		fp.mu.Lock()
		fp.status, fp.body = tc.status, tc.body
		fp.mu.Unlock()
		w := do(h, http.MethodDelete, "/api/v1/me", bearer)
		if w.Code != tc.code || answer(t, w)[tc.key] != tc.want {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body)
		}
		if tc.mustNotLeak != "" && strings.Contains(w.Body.String(), tc.mustNotLeak) {
			t.Errorf("%s: portal-api's words reach the person: %s", tc.name, w.Body)
		}
		fp.take()
	}

	// Asked nothing: no bearer, a stream token, the bearer in the URL.
	for name, req := range map[string]struct {
		path   string
		header http.Header
		code   int
	}{
		"no bearer":             {"/api/v1/me", nil, http.StatusUnauthorized},
		"a stream token":        {"/api/v1/me?stream=" + streamToken(t), nil, http.StatusUnauthorized},
		"the bearer in the URL": {"/api/v1/me?token=" + kid, nil, http.StatusBadRequest},
	} {
		if w := do(h, http.MethodDelete, req.path, req.header); w.Code != req.code {
			t.Errorf("%s: %d %s, want %d", name, w.Code, w.Body, req.code)
		}
		if calls := fp.take(); len(calls) != 0 {
			t.Errorf("%s: portal-api was asked", name)
		}
	}

	// Without the token — an external provider's accounts — it is not
	// available, and nothing is asked or deleted.
	h = accountRouter(t, is, fp.URL, "", nil)
	if w := do(h, http.MethodDelete, "/api/v1/me", bearer); w.Code != http.StatusNotImplemented || answer(t, w)["error"] != "account_deletion_unavailable" {
		t.Errorf("no token: %d %s", w.Code, w.Body)
	}
	if calls := fp.take(); len(calls) != 0 {
		t.Error("portal-api was asked without a token")
	}
}

// An account's data, for portal-api: an admin's bearer and the deletion
// token, both, or nothing.
func TestAnAccountsDataTakesAnAdminAndTheToken(t *testing.T) {
	is := newIssuer(t)
	h := accountRouter(t, is, "http://portal-api.invalid", deletionToken, nil)
	admin := http.Header{"Authorization": {"Bearer " + is.tokenWith(t, "admin-1", "chino", realm("zaentrum-admin", "zaentrum-user"))}}
	viewer := http.Header{"Authorization": {"Bearer " + is.tokenWith(t, "kid-1", "chino", realm("zaentrum-user"))}}
	with := func(h http.Header, token string) http.Header {
		out := h.Clone()
		if out == nil {
			out = http.Header{}
		}
		if token != "" {
			out.Set(portal.DeletionHeader, token)
		}
		return out
	}
	path := "/api/v1/admin/accounts/kid-1/data"
	for name, c := range map[string]struct {
		header http.Header
		code   int
	}{
		"no bearer":                     {with(nil, deletionToken), http.StatusUnauthorized},
		"a viewer with the token":       {with(viewer, deletionToken), http.StatusForbidden},
		"an admin without the token":    {with(admin, ""), http.StatusForbidden},
		"an admin with a wrong token":   {with(admin, deletionToken+"x"), http.StatusForbidden},
		"an admin with a shorter token": {with(admin, deletionToken[:20]), http.StatusForbidden},
		"an admin with the token":       {with(admin, deletionToken), http.StatusOK},
	} {
		if w := do(h, http.MethodDelete, path, c.header); w.Code != c.code {
			t.Errorf("%s: %d %s, want %d", name, w.Code, w.Body, c.code)
		}
	}
	h = accountRouter(t, is, "http://portal-api.invalid", "", nil)
	if w := do(h, http.MethodDelete, path, with(admin, deletionToken)); w.Code != http.StatusNotImplemented {
		t.Errorf("no token configured: %d", w.Code)
	}
}

// Against Postgres (CHINO_API_TEST_DATABASE_URL): a refused account deletion
// keeps every row of the person, a done one takes them all, and nobody
// else's.
func TestAccountDeletionKeepsOrTakesTheRows(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	for _, user := range []string{"kid-2", "parent-2"} {
		if err := st.SaveProgress(ctx, user, "m1", 600, 5400); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkWatched(ctx, user, "m2"); err != nil {
			t.Fatal(err)
		}
		if err := st.SetFlag(ctx, store.LikesTable, user, "m1", true); err != nil {
			t.Fatal(err)
		}
		list, err := st.EnsureDefaultList(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.AddWatchlistItem(ctx, user, list, "m3"); err != nil {
			t.Fatal(err)
		}
	}
	held := func(user string) int {
		n := 0
		pos, _ := st.GetProgress(ctx, user, "m1")
		if pos > 0 {
			n++
		}
		watched, _ := st.ListWatched(ctx, user, 10, 0)
		likes, _ := st.ListFlag(ctx, store.LikesTable, user, 10)
		// Reading the lists makes an empty default one again, as for any
		// new viewer: what counts is what is in them.
		lists, _ := st.ListWatchlists(ctx, user)
		for _, l := range lists {
			n += l.ItemCount
		}
		return n + len(watched) + len(likes)
	}

	is := newIssuer(t)
	fp := newFakePortal(t)
	h := accountRouter(t, is, fp.URL, deletionToken, st)
	bearer := http.Header{"Authorization": {"Bearer " + is.tokenWith(t, "kid-2", "chino", realm("zaentrum-user"))}}

	fp.status, fp.body = http.StatusConflict, "You are the last admin: make someone else an admin first."
	if w := do(h, http.MethodDelete, "/api/v1/me", bearer); w.Code != http.StatusConflict {
		t.Fatalf("refused: %d %s", w.Code, w.Body)
	}
	if n := held("kid-2"); n != 4 {
		t.Errorf("a refused deletion took rows: %d of 4 kept", n)
	}
	fp.status, fp.body = http.StatusNoContent, ""
	w := do(h, http.MethodDelete, "/api/v1/me", bearer)
	if w.Code != http.StatusOK {
		t.Fatalf("deleted: %d %s", w.Code, w.Body)
	}
	deleted, _ := answer(t, w)["deleted"].(map[string]any)
	if deleted["progress"] != float64(1) || deleted["watched"] != float64(1) || deleted["likes"] != float64(1) ||
		deleted["watchlists"] != float64(1) || deleted["watchlistItems"] != float64(1) {
		t.Errorf("counted %v", deleted)
	}
	if n := held("kid-2"); n != 0 {
		t.Errorf("%d rows of the deleted person stay", n)
	}
	if n := held("parent-2"); n != 4 {
		t.Errorf("another person's rows: %d of 4", n)
	}
}
