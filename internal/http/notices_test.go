package http

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// noticePortal is portal-api's /api/portal/me/notices routes: it records
// what each call carried and answers each route what the test says.
type noticePortal struct {
	*httptest.Server
	mu    sync.Mutex
	calls []seenCall
	// answers by "METHOD path": status and body; the default is 404.
	answers map[string]portalAnswer
}

type seenCall struct{ method, path, auth string }

type portalAnswer struct {
	status int
	body   string
}

const miasNotices = `{"notices":[
	{"id":"0f0e0d0c-0b0a-4000-8000-000000000001","addon":"example","addonTitle":"Example","addonIcon":"puzzle",
	 "title":"Your title is ready","body":"It is in your library now.","link":"https://media.example.org/portal/app/example",
	 "itemId":"item-1","createdAt":"2026-10-05T07:58:00Z","readAt":null,"unknownField":"dropped"},
	{"id":"0f0e0d0c-0b0a-4000-8000-000000000002","addon":"example","addonTitle":"Example","addonIcon":"puzzle",
	 "title":"Hello","body":"Plain text.","link":"","itemId":"","createdAt":"2026-10-04T07:58:00Z","readAt":"2026-10-04T08:00:00Z"}
],"unread":1}`

func newNoticePortal(t *testing.T) *noticePortal {
	t.Helper()
	p := &noticePortal{answers: map[string]portalAnswer{
		"GET /api/portal/me/notices":                                            {http.StatusOK, miasNotices},
		"POST /api/portal/me/notices/read-all":                                  {http.StatusOK, `{"read":1,"unread":0}`},
		"POST /api/portal/me/notices/0f0e0d0c-0b0a-4000-8000-000000000001/read": {http.StatusOK, `{"unread":0}`},
		"DELETE /api/portal/me/notices/0f0e0d0c-0b0a-4000-8000-000000000002":    {http.StatusNoContent, ""},
	}}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		p.mu.Lock()
		defer p.mu.Unlock()
		p.calls = append(p.calls, seenCall{r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization")})
		a, ok := p.answers[r.Method+" "+r.URL.EscapedPath()]
		if !ok {
			http.Error(w, "no such notice", http.StatusNotFound)
			return
		}
		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(a.body))
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *noticePortal) take() []seenCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.calls
	p.calls = nil
	return out
}

func (p *noticePortal) answer(route string, status int, body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answers[route] = portalAnswer{status, body}
}

type noticesAnswer struct {
	Notices   []map[string]any `json:"notices"`
	Unread    int              `json:"unread"`
	Available bool             `json:"available"`
}

func readNotices(t *testing.T, w *httptest.ResponseRecorder) noticesAnswer {
	t.Helper()
	var v noticesAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("not JSON: %s", w.Body)
	}
	return v
}

// GET /api/v1/notices reads the viewer's notices from portal-api with their
// own bearer — the header's, or the deprecated ?token= — and serves them as
// portal-api has them, the fields it knows and no others.
func TestNoticesAreTheViewersOwnFromPortalAPI(t *testing.T) {
	is := newIssuer(t)
	fp := newNoticePortal(t)
	h := accountRouter(t, is, fp.URL, "", nil)
	mia := is.tokenWith(t, "user-mia", "chino", realm("zaentrum-user"))

	w := do(h, http.MethodGet, "/api/v1/notices", http.Header{"Authorization": {"Bearer " + mia}})
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	got := readNotices(t, w)
	if !got.Available || got.Unread != 1 || len(got.Notices) != 2 {
		t.Fatalf("answered %s", w.Body)
	}
	first := got.Notices[0]
	for k, want := range map[string]any{
		"id": "0f0e0d0c-0b0a-4000-8000-000000000001", "addon": "example", "addonTitle": "Example", "addonIcon": "puzzle",
		"title": "Your title is ready", "body": "It is in your library now.", "link": "https://media.example.org/portal/app/example",
		"itemId": "item-1", "createdAt": "2026-10-05T07:58:00Z", "readAt": nil,
	} {
		if first[k] != want {
			t.Errorf("%s = %v, want %v", k, first[k], want)
		}
	}
	if _, ok := first["unknownField"]; ok {
		t.Error("a field chino-api does not know was passed on")
	}
	if got.Notices[1]["readAt"] != "2026-10-04T08:00:00Z" {
		t.Errorf("read = %v", got.Notices[1]["readAt"])
	}
	calls := fp.take()
	if len(calls) != 1 || calls[0].method != http.MethodGet || calls[0].path != "/api/portal/me/notices" || calls[0].auth != "Bearer "+mia {
		t.Errorf("portal-api was asked %+v", calls)
	}

	// The deprecated bearer in the query is the viewer's too.
	if w := do(h, http.MethodGet, "/api/v1/notices?token="+mia, nil); w.Code != http.StatusOK || !readNotices(t, w).Available {
		t.Errorf("?token= = %d %s", w.Code, w.Body)
	}
	if calls := fp.take(); len(calls) != 1 || calls[0].auth != "Bearer "+mia {
		t.Errorf("with ?token=, portal-api was asked %+v", calls)
	}

	// No bearer, no stream token: chino-api asks nothing.
	if w := do(h, http.MethodGet, "/api/v1/notices", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("no bearer = %d", w.Code)
	}
	if w := do(h, http.MethodGet, "/api/v1/notices?stream="+streamToken(t), nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a stream token = %d", w.Code)
	}
	if calls := fp.take(); len(calls) != 0 {
		t.Errorf("portal-api was asked %+v", calls)
	}
}

// A home screen that shows notices never fails for them: no portal-api, one
// that is down, refuses or answers nonsense — the list is empty, available
// false, and the answer 200.
func TestNoticesAreBestEffort(t *testing.T) {
	is := newIssuer(t)
	mia := http.Header{"Authorization": {"Bearer " + is.tokenWith(t, "user-mia", "chino", realm("zaentrum-user"))}}
	// Nothing listens on port 1: a closed test server's port could be
	// handed to the next fake portal-api.
	const gone = "http://127.0.0.1:1"
	for name, setup := range map[string]func(*noticePortal) string{
		"no portal-api configured": func(*noticePortal) string { return "" },
		"portal-api down":          func(*noticePortal) string { return gone },
		"portal-api failing": func(p *noticePortal) string {
			p.answer("GET /api/portal/me/notices", http.StatusInternalServerError, "boom")
			return p.URL
		},
		"portal-api refusing the bearer": func(p *noticePortal) string {
			p.answer("GET /api/portal/me/notices", http.StatusUnauthorized, "unauthorized")
			return p.URL
		},
		"an older portal-api without notices": func(p *noticePortal) string {
			p.answer("GET /api/portal/me/notices", http.StatusNotFound, "404 page not found")
			return p.URL
		},
		"portal-api answering nonsense": func(p *noticePortal) string {
			p.answer("GET /api/portal/me/notices", http.StatusOK, "<html>a login page</html>")
			return p.URL
		},
	} {
		fp := newNoticePortal(t)
		h := accountRouter(t, is, setup(fp), "", nil)
		w := do(h, http.MethodGet, "/api/v1/notices", mia)
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"available":false,"notices":[],"unread":0}` {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
}

// Marking read and deleting go to portal-api with the viewer's bearer; a
// notice the viewer does not have is 404, as portal-api says; one portal-api
// cannot change is 502 — 503 with no portal-api — and nothing that reads as
// more than an id is forwarded.
func TestNoticeChangesAreForwarded(t *testing.T) {
	is := newIssuer(t)
	fp := newNoticePortal(t)
	h := accountRouter(t, is, fp.URL, "", nil)
	mia := is.tokenWith(t, "user-mia", "chino", realm("zaentrum-user"))
	bearer := http.Header{"Authorization": {"Bearer " + mia}}

	for _, c := range []struct {
		method, path string
		code         int
		body         string
		portal       string
	}{
		{http.MethodPost, "/api/v1/notices/0f0e0d0c-0b0a-4000-8000-000000000001/read", http.StatusOK, `{"unread":0}`,
			"POST /api/portal/me/notices/0f0e0d0c-0b0a-4000-8000-000000000001/read"},
		{http.MethodPost, "/api/v1/notices/read-all", http.StatusOK, `{"read":1,"unread":0}`, "POST /api/portal/me/notices/read-all"},
		{http.MethodDelete, "/api/v1/notices/0f0e0d0c-0b0a-4000-8000-000000000002", http.StatusNoContent, "",
			"DELETE /api/portal/me/notices/0f0e0d0c-0b0a-4000-8000-000000000002"},
		{http.MethodPost, "/api/v1/notices/0f0e0d0c-0b0a-4000-8000-00000000dead/read", http.StatusNotFound, `"error":"not_found"`,
			"POST /api/portal/me/notices/0f0e0d0c-0b0a-4000-8000-00000000dead/read"},
		{http.MethodDelete, "/api/v1/notices/someone-elses", http.StatusNotFound, `"error":"not_found"`,
			"DELETE /api/portal/me/notices/someone-elses"},
	} {
		w := do(h, c.method, c.path, bearer)
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.body) {
			t.Errorf("%s %s = %d %s, want %d %s", c.method, c.path, w.Code, w.Body, c.code, c.body)
		}
		calls := fp.take()
		if len(calls) != 1 || calls[0].method+" "+calls[0].path != c.portal || calls[0].auth != "Bearer "+mia {
			t.Errorf("%s %s: portal-api was asked %+v", c.method, c.path, calls)
		}
	}

	// What reads as a path, not an id, never reaches portal-api.
	for _, path := range []string{
		"/api/v1/notices/..%2F..%2Fslots%2Fsearch.empty/read",
		"/api/v1/notices/a.b/read",
		"/api/v1/notices/%2e%2e",
		"/api/v1/notices/" + strings.Repeat("a", 65),
	} {
		method := http.MethodPost
		if !strings.HasSuffix(path, "/read") {
			method = http.MethodDelete
		}
		if w := do(h, method, path, bearer); w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d %s", method, path, w.Code, w.Body)
		}
	}
	if calls := fp.take(); len(calls) != 0 {
		t.Errorf("portal-api was asked %+v", calls)
	}

	// portal-api failing: 502, in chino-api's words.
	fp.answer("POST /api/portal/me/notices/read-all", http.StatusInternalServerError, "pq: relation does not exist")
	if w := do(h, http.MethodPost, "/api/v1/notices/read-all", bearer); w.Code != http.StatusBadGateway ||
		!strings.Contains(w.Body.String(), "notices_unavailable") || strings.Contains(w.Body.String(), "pq:") {
		t.Errorf("portal-api failing = %d %s", w.Code, w.Body)
	}
	fp.take()
	// No portal-api: 503.
	h = accountRouter(t, is, "", "", nil)
	if w := do(h, http.MethodDelete, "/api/v1/notices/0f0e0d0c-0b0a-4000-8000-000000000002", bearer); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no portal-api = %d %s", w.Code, w.Body)
	}
	// No bearer: 401, nothing asked.
	h = accountRouter(t, is, fp.URL, "", nil)
	if w := do(h, http.MethodPost, "/api/v1/notices/read-all", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("no bearer = %d", w.Code)
	}
	if calls := fp.take(); len(calls) != 0 {
		t.Errorf("portal-api was asked %+v", calls)
	}
}
