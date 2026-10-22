package http

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// languagesRoute is chino-api's route of the instance's subtitle languages.
const languagesRoute = "/api/v1/subtitles/languages"

// languagesUnavailable is the body of its 502.
const languagesUnavailable = "subtitle languages unavailable\n"

// threeLanguages is katalog-manager's answer of three languages, and
// threeLanguagesOut the route's.
const (
	threeLanguages    = `{"from":"subtitles.languages","languages":["en","de","zh-Hans"]}`
	threeLanguagesOut = `{"languages":["en","de","zh-Hans"]}` + "\n"
)

// languagesManager is a katalog-manager whose answer of the subtitle
// languages a test sets, recording each request it gets: its method, its path
// and query, and its Authorization.
type languagesManager struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	body   string
	stall  bool
	reqs   []string
}

func newLanguagesManager(t *testing.T, body string) *languagesManager {
	t.Helper()
	m := &languagesManager{status: http.StatusOK, body: body}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.reqs = append(m.reqs, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
		status, body, stall := m.status, m.body, m.stall
		m.mu.Unlock()
		if r.URL.Path != languagesPath {
			http.NotFound(w, r)
			return
		}
		if stall { // until chino-api gives up
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(m.Close)
	return m
}

// answer is what the manager answers from now on.
func (m *languagesManager) answer(status int, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status, m.body, m.stall = status, body, false
}

// stalls has the manager answer nothing from now on.
func (m *languagesManager) stalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stall = true
}

// take is the requests the manager got since the last take.
func (m *languagesManager) take() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.reqs
	m.reqs = nil
	return out
}

// languagesRequest is the request katalog-manager gets with bearer.
func languagesRequest(bearer string) string {
	return "GET " + languagesPath + " Bearer " + bearer
}

func bearerHeader(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

// GET /api/v1/subtitles/languages is katalog-manager's languages, asked with
// the viewer's bearer: the tags as it gives them, in its order, and nothing
// else - not the setting they come from. A bearer in the deprecated ?token=
// goes to katalog-manager in the header, and no query goes with it.
func TestTheSubtitleLanguagesAreKatalogManagers(t *testing.T) {
	is := newIssuer(t)
	manager := newLanguagesManager(t, threeLanguages)
	viewer := is.token(t, "viewer-1", "chino")
	for _, tc := range []struct {
		name, query string
		header      http.Header
	}{
		{"the bearer in the header", "", bearerHeader(viewer)},
		{"the bearer in ?token=", "?token=" + viewer, nil},
	} {
		h := adminRouter(t, is, manager.URL, "http://katalog-api.invalid", nil)
		w := do(h, "GET", languagesRoute+tc.query, tc.header)
		if w.Code != http.StatusOK || w.Body.String() != threeLanguagesOut || w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d %q (%s), want 200 %q", tc.name, w.Code, w.Body, w.Header().Get("Content-Type"), threeLanguagesOut)
		}
		if got := manager.take(); len(got) != 1 || got[0] != languagesRequest(viewer) {
			t.Errorf("%s: katalog-manager got %q, want %q", tc.name, got, languagesRequest(viewer))
		}
	}
}

// With no languages set katalog-manager answers [] and from null, and the
// route [] - never null.
func TestTheSubtitleLanguagesAreEmptyWhenNoneIsSet(t *testing.T) {
	is := newIssuer(t)
	manager := newLanguagesManager(t, `{"from":null,"languages":[]}`)
	h := adminRouter(t, is, manager.URL, "http://katalog-api.invalid", nil)
	w := do(h, "GET", languagesRoute, bearerHeader(is.token(t, "viewer-1", "chino")))
	if want := `{"languages":[]}` + "\n"; w.Code != http.StatusOK || w.Body.String() != want {
		t.Errorf("none set: %d %q, want 200 %q", w.Code, w.Body, want)
	}
}

// The languages are the instance's: the answer one viewer's request got is
// kept for every viewer, and within the five minutes katalog-manager is not
// asked again, also when it would answer otherwise by now. Viewers asking at
// once while nothing is kept each get the list.
func TestTheSubtitleLanguagesAreKeptForEveryViewer(t *testing.T) {
	is := newIssuer(t)
	manager := newLanguagesManager(t, threeLanguages)
	h := adminRouter(t, is, manager.URL, "http://katalog-api.invalid", nil)
	viewers := make([]http.Header, 10)
	for i := range viewers {
		viewers[i] = bearerHeader(is.token(t, fmt.Sprintf("viewer-%d", i), "chino"))
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := do(h, "GET", languagesRoute, viewers[i]); w.Code != http.StatusOK || w.Body.String() != threeLanguagesOut {
				t.Errorf("viewer %d of those at once: %d %q", i, w.Code, w.Body)
			}
		}()
	}
	wg.Wait()
	if n := len(manager.take()); n < 1 || n > 8 {
		t.Errorf("the viewers at once asked katalog-manager %d times", n)
	}

	manager.answer(http.StatusOK, `{"from":"subtitles.languages","languages":["fr"]}`)
	for i := 8; i < 10; i++ {
		if w := do(h, "GET", languagesRoute, viewers[i]); w.Code != http.StatusOK || w.Body.String() != threeLanguagesOut {
			t.Errorf("viewer %d: %d %q, want the kept answer", i, w.Code, w.Body)
		}
	}
	if got := manager.take(); len(got) != 0 {
		t.Errorf("with an answer kept, katalog-manager got %q", got)
	}
}

// An answer is fresh for five minutes; then the next request asks
// katalog-manager again, and serves and keeps what it answers. When it does
// not answer then, the last answer is served while it is less than an hour
// old - a failure does not make it younger - and after that the answer is 502,
// until katalog-manager answers again. While no fresh answer is kept every
// request asks: a failure is not kept.
func TestTheLastSubtitleLanguagesAreServedForAnHour(t *testing.T) {
	manager := newLanguagesManager(t, threeLanguages)
	s := newSubtitleLanguages(manager.URL)
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := start
	s.now = func() time.Time { return now }
	german := `{"languages":["de"]}` + "\n"
	down := func() { manager.answer(http.StatusInternalServerError, "the settings could not be read") }
	for _, step := range []struct {
		at   time.Duration
		then func() // what katalog-manager answers from then on
		code int
		body string
		asks int
	}{
		{0, nil, 200, threeLanguagesOut, 1},
		{4*time.Minute + 59*time.Second, nil, 200, threeLanguagesOut, 0},
		{5 * time.Minute, func() { manager.answer(http.StatusOK, `{"from":"subtitles.languages","languages":["de"]}`) }, 200, german, 1},
		{9*time.Minute + 59*time.Second, nil, 200, german, 0},
		{10 * time.Minute, down, 200, german, 1},
		{11 * time.Minute, nil, 200, german, 1},
		{time.Hour + 4*time.Minute + 59*time.Second, nil, 200, german, 1},
		{time.Hour + 5*time.Minute, nil, 502, languagesUnavailable, 1},
		{time.Hour + 5*time.Minute + time.Second, nil, 502, languagesUnavailable, 1},
		{time.Hour + 6*time.Minute, func() { manager.answer(http.StatusOK, threeLanguages) }, 200, threeLanguagesOut, 1},
		{time.Hour + 10*time.Minute + 59*time.Second, nil, 200, threeLanguagesOut, 0},
	} {
		now = start.Add(step.at)
		if step.then != nil {
			step.then()
		}
		w := do(s, "GET", languagesRoute, bearerHeader("viewer-token"))
		asks := len(manager.take())
		if w.Code != step.code || w.Body.String() != step.body || asks != step.asks {
			t.Errorf("at %v: %d %q, katalog-manager asked %d times; want %d %q, asked %d times",
				step.at, w.Code, w.Body, asks, step.code, step.body, step.asks)
		}
	}
}

// When katalog-manager does not answer - none is configured, it is not there,
// it takes longer than the timeout, it answers a status other than 2xx, or a
// body that is not its answer - and nothing is kept, the answer is 502 and no
// list. The failure is not kept: the next request asks again, and serves what
// katalog-manager answers then.
func TestNoSubtitleLanguagesWhenKatalogManagerDoesNotAnswer(t *testing.T) {
	refusedTo := func(what string, s http.Handler, header http.Header) {
		t.Helper()
		if w := do(s, "GET", languagesRoute, header); w.Code != http.StatusBadGateway || w.Body.String() != languagesUnavailable {
			t.Errorf("%s: %d %q, want 502 %q", what, w.Code, w.Body, languagesUnavailable)
		}
	}
	refused := func(what string, s http.Handler) {
		t.Helper()
		refusedTo(what, s, bearerHeader("viewer-token"))
	}
	for _, tc := range []struct {
		name   string
		status int // 0: no answer
		body   string
	}{
		{"a 500", http.StatusInternalServerError, `{"error":"the settings could not be read"}`},
		{"a 403: the viewer refused", http.StatusForbidden, `{"error":"forbidden"}`},
		{"a 204", http.StatusNoContent, ``},
		{"no JSON", http.StatusOK, `<html>sign in</html>`},
		{"no languages", http.StatusOK, `{"from":null}`},
		{"languages null", http.StatusOK, `{"from":null,"languages":null}`},
		{"languages that are no list", http.StatusOK, `{"languages":"en,de"}`},
		{"languages that are not strings", http.StatusOK, `{"languages":["en",7]}`},
		{"a list alone", http.StatusOK, `["en","de"]`},
		{"no answer in time", 0, ``},
	} {
		manager := newLanguagesManager(t, tc.body)
		s := newSubtitleLanguages(manager.URL)
		if tc.status == 0 {
			manager.stalls()
			s.timeout = 100 * time.Millisecond
		} else {
			manager.answer(tc.status, tc.body)
		}
		refused(tc.name, s)
		s.timeout = languagesTimeout
		manager.answer(http.StatusOK, threeLanguages)
		if w := do(s, "GET", languagesRoute, bearerHeader("viewer-token")); w.Code != http.StatusOK || w.Body.String() != threeLanguagesOut {
			t.Errorf("%s, then the languages: %d %q, want 200 %q", tc.name, w.Code, w.Body, threeLanguagesOut)
		}
		if n := len(manager.take()); n != 2 {
			t.Errorf("%s: katalog-manager asked %d times, want 2", tc.name, n)
		}
	}

	refused("no katalog-manager configured", newSubtitleLanguages(""))
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	refused("katalog-manager not there", newSubtitleLanguages(gone.URL))
	is := newIssuer(t)
	refusedTo("through the router, katalog-manager not there",
		adminRouter(t, is, gone.URL, "http://katalog-api.invalid", nil), bearerHeader(is.token(t, "viewer-1", "chino")))
}

// Whom katalog-manager answers is its to decide. One that answers an admin's
// bearer alone refuses a viewer's: the viewer gets 502 while nothing is kept,
// and the list once an admin's request has kept it.
func TestTheSubtitleLanguagesOfAKatalogManagerThatAnswersAdminsAlone(t *testing.T) {
	is := newIssuer(t)
	admin := is.tokenWith(t, "admin-1", "chino", realm("zaentrum-admin"))
	viewer := is.token(t, "viewer-1", "chino")
	manager := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+admin {
			http.Error(w, `{"error":"admin, service account or addon access required"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, threeLanguages)
	})
	h := adminRouter(t, is, manager.URL, "http://katalog-api.invalid", nil)
	for _, step := range []struct {
		who    string
		header http.Header
		code   int
		body   string
		asks   int
	}{
		{"the viewer", bearerHeader(viewer), http.StatusBadGateway, languagesUnavailable, 1},
		{"the admin", bearerHeader(admin), http.StatusOK, threeLanguagesOut, 1},
		{"the viewer again", bearerHeader(viewer), http.StatusOK, threeLanguagesOut, 0},
	} {
		w := do(h, "GET", languagesRoute, step.header)
		if asks := len(manager.take()); w.Code != step.code || w.Body.String() != step.body || asks != step.asks {
			t.Errorf("%s: %d %q, katalog-manager asked %d times; want %d %q, asked %d times",
				step.who, w.Code, w.Body, asks, step.code, step.body, step.asks)
		}
	}
}

// The route is the signed-in viewer's, as the default group's are: without a
// bearer, with a stream token alone, with a token of another audience or a
// bearer that is no token, it is 401, and nothing goes to katalog-manager.
func TestTheSubtitleLanguagesAreForASignedInViewer(t *testing.T) {
	is := newIssuer(t)
	manager := newLanguagesManager(t, threeLanguages)
	h := adminRouter(t, is, manager.URL, "http://katalog-api.invalid", nil)
	for _, tc := range []struct {
		name, query string
		header      http.Header
	}{
		{"no credential", "", nil},
		{"a stream token", "?stream=" + streamToken(t), nil},
		{"a token of another audience", "", bearerHeader(is.token(t, "viewer-1", "elsewhere"))},
		{"a bearer that is no token", "", bearerHeader("not-a-token")},
	} {
		if w := do(h, "GET", languagesRoute+tc.query, tc.header); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d %q, want 401", tc.name, w.Code, w.Body)
		}
	}
	if got := manager.take(); len(got) != 0 {
		t.Errorf("katalog-manager got %q for callers refused", got)
	}
}

// Of two requests asking at once, the answer to the later question is kept:
// one asked for earlier does not replace it.
func TestAnEarlierAnswerDoesNotReplaceALaterOne(t *testing.T) {
	s := newSubtitleLanguages("")
	at := time.Now()
	s.keep([]string{"de"}, at.Add(time.Second))
	s.keep([]string{"en"}, at)
	if langs, _, ok := s.kept(time.Hour); !ok || len(langs) != 1 || langs[0] != "de" {
		t.Errorf("kept %q (%v), want the later answer, de", langs, ok)
	}
}
