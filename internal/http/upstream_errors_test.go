package http

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
)

// upstreamText is what the fake upstreams fail with: none of it may reach a
// caller.
const upstreamText = `pq: relation "items" does not exist (SQLSTATE 42P01)`

// failingUpstream is an upstream that fails every request with 500 and
// upstreamText.
func failingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, upstreamText, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// hanging is an upstream that answers nothing until its request is given up.
func hanging(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// logBuffer is what chino-api logs while a test runs, read and written under
// one lock: a server's goroutines write to it.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logBuffer) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Reset()
}

// lines are the lines logged.
func (l *logBuffer) lines() []string {
	return strings.FieldsFunc(l.String(), func(r rune) bool { return r == '\n' })
}

// logs is what chino-api logs while the test runs.
func logs(t *testing.T) *logBuffer {
	t.Helper()
	b := &logBuffer{}
	prev := log.Writer()
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(prev) })
	return b
}

// loggedOnce fails t unless what was logged is one line, and that line holds each
// of want.
func loggedOnce(t *testing.T, what string, logged *logBuffer, want ...string) {
	t.Helper()
	lines := logged.lines()
	if len(lines) != 1 {
		t.Errorf("%s: logged %d lines, want 1: %q", what, len(lines), lines)
		return
	}
	for _, w := range want {
		if !strings.Contains(lines[0], w) {
			t.Errorf("%s: logged %q, want %q in it", what, lines[0], w)
		}
	}
}

// gone is a request of method to path whose viewer has gone.
func gone(method, path string, header http.Header) *http.Request {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(method, path, nil).WithContext(ctx)
	for k, v := range header {
		r.Header[k] = v
	}
	return r
}

// halfClosed sends a request of method to path, with header, to srv on a
// connection of its own, closes its side of the connection for writing, as a
// client that has gone does, and reads the answer to its end.
func halfClosed(t *testing.T, srv *httptest.Server, method, path string, header http.Header) (int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	req := method + " " + path + " HTTP/1.1\r\nHost: example.com\r\n"
	for k, vs := range header {
		for _, v := range vs {
			req += k + ": " + v + "\r\n"
		}
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("%s %s: no answer: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// katalog-api failing a route: 502 "catalog unavailable", as plain text, or
// as the code of the lists and search with a fixed detail; katalog-api's
// error is in the log, once, with the method and the route, and in no
// answer. A request whose viewer went is answered the same, and logged once
// as the viewer having gone.
func TestACatalogFailureAnswersAFixedLine(t *testing.T) {
	h := router(t, failingUpstream(t).URL, "http://katalog-manager.invalid", false)
	logged := logs(t)
	plain := errCatalog + "\n"
	code := `{"detail":"` + catalogDetail + `","error":"` + errCatalog + `","product":"chino"}` + "\n"
	for _, c := range []struct{ path, route, want string }{
		{"/api/v1/genres", "/api/v1/genres", plain},
		{"/api/v1/people?q=ada", "/api/v1/people", plain},
		{"/api/v1/people/p1", "/api/v1/people/{id}", plain},
		{"/api/v1/series/s1/episodes", "/api/v1/series/{id}/episodes", plain},
		{"/api/v1/series/s1/next-episode", "/api/v1/series/{id}/next-episode", plain},
		{"/api/v1/items/i1/segments", "/api/v1/items/{id}/segments", plain},
		{"/api/v1/items/i1/similar", "/api/v1/items/{id}/similar", plain},
		{"/api/v1/items/i1/subtitles", "/api/v1/items/{id}/subtitles", plain},
		{"/api/v1/items/i1", "/api/v1/items/{id}", plain},
		{"/api/v1/items?type=movie", "/api/v1/items", code},
		{"/api/v1/search?q=ada", "/api/v1/search", code},
	} {
		logged.Reset()
		w := do(h, http.MethodGet, c.path, nil)
		if w.Code != http.StatusBadGateway || w.Body.String() != c.want {
			t.Errorf("%s: %d %q, want 502 %q", c.path, w.Code, w.Body, c.want)
		}
		loggedOnce(t, c.path, logged, "GET "+c.route+`: katalog-api failed: "katalog`, ": 500")

		logged.Reset()
		w = httptest.NewRecorder()
		h.ServeHTTP(w, gone(http.MethodGet, c.path, nil))
		if w.Code != http.StatusBadGateway || w.Body.String() != c.want {
			t.Errorf("%s, the viewer gone: %d %q, want 502 %q", c.path, w.Code, w.Body, c.want)
		}
		loggedOnce(t, c.path+", the viewer gone", logged, "GET "+c.route+": the viewer went before katalog-api answered")
	}
}

// A failing database answers 500 "database error" and logs the error once,
// also where it did not answer in time, and where the viewer went, as such.
func TestADatabaseFailureIsAnsweredAndLoggedOnce(t *testing.T) {
	logged := logs(t)
	late, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	for _, c := range []struct {
		name string
		r    *http.Request
		err  error
		log  string
	}{
		{"the database failed", httptest.NewRequest(http.MethodGet, settingsRoute, nil), errors.New("conn closed"),
			`GET /api/v1/me/settings: the database failed: "conn closed"`},
		{"the database did not answer in time", httptest.NewRequest(http.MethodGet, settingsRoute, nil).WithContext(late),
			context.DeadlineExceeded, `GET /api/v1/me/settings: the database failed: "context deadline exceeded"`},
		{"the viewer gone", gone(http.MethodGet, settingsRoute, nil), context.Canceled,
			"GET /api/v1/me/settings: the viewer went before the database answered"},
	} {
		logged.Reset()
		mux := chi.NewRouter()
		mux.Get(settingsRoute, func(w http.ResponseWriter, r *http.Request) { storeFailed(w, r, c.err) })
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, c.r)
		if w.Code != http.StatusInternalServerError || w.Body.String() != errDatabase+"\n" {
			t.Errorf("%s: %d %q, want 500 %q", c.name, w.Code, w.Body, errDatabase+"\n")
		}
		loggedOnce(t, c.name, logged, c.log)
	}
}

// Over a real connection, a client that half-closes after its request and
// keeps reading is answered the failure's status and line, not an empty
// 200: the database's, katalog-api's on a route and on the cap gate,
// chino-stream's on the media proxy, and a list's kinds; each is logged once,
// as the viewer having gone before the answer, with the route.
func TestAFailureIsAnsweredToAViewerWhoWent(t *testing.T) {
	logged := logs(t)
	mux := chi.NewRouter()
	mux.Get(settingsRoute, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		storeFailed(w, r, r.Context().Err())
	})
	db := httptest.NewServer(mux)
	t.Cleanup(db.Close)
	if code, body := halfClosed(t, db, http.MethodGet, settingsRoute, nil); code != http.StatusInternalServerError || body != errDatabase+"\n" {
		t.Errorf("the database: %d %q, want 500 %q", code, body, errDatabase+"\n")
	}
	loggedOnce(t, "the database", logged, "GET /api/v1/me/settings: the viewer went before the database answered")

	is := newIssuer(t)
	up := hanging(t)
	h, err := NewRouter(config.Config{
		OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: up.URL, StreamBaseURL: up.URL, ArtworkBaseURL: up.URL, StreamSigningKey: signingKey,
	}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	viewer := bearerWith(t, is, "viewer-1", nil)
	kid := bearerWith(t, is, "kid-1", 12)
	// The issuer's keys asked once, before anyone goes.
	if w := do(h, http.MethodGet, "/api/v1/me", viewer); w.Code != http.StatusOK {
		t.Fatalf("GET /me: %d %q", w.Code, w.Body)
	}
	for _, c := range []struct {
		name, path string
		header     http.Header
		code       int
		body, log  string
	}{
		{"a route", "/api/v1/genres", viewer, http.StatusBadGateway, errCatalog + "\n",
			"GET /api/v1/genres: the viewer went before katalog-api answered"},
		{"the lists", "/api/v1/items?type=movie", viewer, http.StatusBadGateway,
			`{"detail":"` + catalogDetail + `","error":"` + errCatalog + `","product":"chino"}` + "\n",
			"GET /api/v1/items: the viewer went before katalog-api answered"},
		{"the cap gate", "/api/v1/items/i1", kid, http.StatusBadGateway, errCatalog + "\n",
			"GET /api/v1/items/{id}: the viewer went before katalog-api answered (what the viewer may be served)"},
		{"the media proxy", "/api/v1/items/i1/play/info?stream=" + streamToken(t), nil, http.StatusBadGateway,
			"playback unavailable\n", "GET /api/v1/items/{id}/play/info: the viewer went before chino-stream answered"},
	} {
		logged.Reset()
		if code, body := halfClosed(t, srv, http.MethodGet, c.path, c.header); code != c.code || body != c.body {
			t.Errorf("%s: %d %q, want %d %q", c.name, code, body, c.code, c.body)
		}
		loggedOnce(t, c.name, logged, c.log)
	}

	logged.Reset()
	w := httptest.NewRecorder()
	kindsUnavailable(w, gone(http.MethodGet, "/api/v1/me/watchlists/wl_1?kind=movie", nil), context.Canceled)
	if w.Code != http.StatusBadGateway || w.Body.String() != errCatalog+"\n" {
		t.Errorf("a list's kinds: %d %q", w.Code, w.Body)
	}
	loggedOnce(t, "a list's kinds", logged, "the viewer went before katalog-api answered (the kinds of a list's titles)")
}

// What a log line carries of a caller or an upstream stays on that one line,
// and is bounded: a path with an encoded line break is logged by its route
// pattern; an oversized bearer's reason, and an upstream's error with a line
// break in it, are quoted and cut; a search's words are not logged.
func TestALogLineIsOneBoundedLine(t *testing.T) {
	logged := logs(t)
	is := newIssuer(t)
	h := oidcRouter(t, is)
	w := do(h, http.MethodGet, "/api/v1/items/i1%0Achino-api:%20forged%20line", http.Header{"Authorization": {"Bearer not-a-token"}})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("a path with a line break: %d %q", w.Code, w.Body)
	}
	loggedOnce(t, "a path with a line break", logged, "GET /api/v1/items/{id}: a bearer was refused: ")
	if strings.Contains(logged.String(), "forged") {
		t.Errorf("a path with a line break: logged %q", logged)
	}

	logged.Reset()
	huge := b64([]byte(`{"alg":"RS256"}`)) + "." + b64([]byte(`{"iss":"`+strings.Repeat("x", 300<<10)+`"}`)) + ".c2ln"
	if w := do(h, http.MethodGet, "/api/v1/me", http.Header{"Authorization": {"Bearer " + huge}}); w.Code != http.StatusUnauthorized {
		t.Errorf("an oversized bearer: %d", w.Code)
	}
	loggedOnce(t, "an oversized bearer", logged, "GET /api/v1/me: a bearer was refused: ")
	if n := len(logged.String()); n > 2048 {
		t.Errorf("an oversized bearer (%d bytes): logged %d bytes", len(huge), n)
	}

	logged.Reset()
	breaking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom\nchino-api: GET /admin: all is well", http.StatusInternalServerError)
	}))
	t.Cleanup(breaking.Close)
	if w := do(router(t, breaking.URL, "http://katalog-manager.invalid", false), http.MethodGet, "/api/v1/people/p1", nil); w.Code != http.StatusBadGateway {
		t.Errorf("an upstream's error with a line break: %d", w.Code)
	}
	loggedOnce(t, "an upstream's error with a line break", logged, `GET /api/v1/people/{id}: katalog-api failed: "katalog person: 500 boom\nchino-api: GET /admin: all is well\n"`)

	unreachable := router(t, "http://katalog-api.invalid", "http://katalog-manager.invalid", false)
	for _, path := range []string{"/api/v1/search?q=secret+words", "/api/v1/items?type=movie&q=secret+words", "/api/v1/people?q=secret+words"} {
		logged.Reset()
		if w := do(unreachable, http.MethodGet, path, nil); w.Code != http.StatusBadGateway {
			t.Errorf("%s: %d", path, w.Code)
		}
		loggedOnce(t, path, logged, "katalog-api failed: ", "q=REDACTED")
		if strings.Contains(logged.String(), "secret") {
			t.Errorf("%s: logged the search's words: %q", path, logged)
		}
	}
}

// The other upstreams: chino-stream and katalog-manager not answering a media
// route are 502 "playback unavailable" and "artwork unavailable", an admin's
// packaging that katalog-manager does not answer 502 "packaging
// unavailable"; their errors are logged once, a viewer who went as such.
// portal-api failing account deletion or a notice's change is answered with
// the code and the line of chino-api's own.
func TestAnUpstreamsFailureKeepsItsTextToTheServer(t *testing.T) {
	is := newIssuer(t)
	portal := failingUpstream(t)
	h, err := NewRouter(config.Config{
		OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: "http://katalog-api.invalid", StreamBaseURL: "http://chino-stream.invalid",
		ArtworkBaseURL: "http://katalog-manager.invalid", KatalogManagerURL: "http://katalog-manager.invalid",
		StreamSigningKey: signingKey, PortalBaseURL: portal.URL, AccountDeletionToken: deletionToken,
	}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	logged := logs(t)
	viewer := http.Header{"Authorization": {"Bearer " + is.tokenWith(t, "viewer-1", "chino", realm("zaentrum-user"))}}
	admin := http.Header{"Authorization": {"Bearer " + is.tokenWith(t, "admin-1", "chino", realm("zaentrum-admin"))}}

	for _, c := range []struct {
		method, path      string
		header            http.Header
		want, log, url    string
		upstream, through string
	}{
		{http.MethodGet, "/api/v1/items/i1/play/info?stream=" + streamToken(t), nil, "playback unavailable\n",
			"GET /api/v1/items/{id}/play/info: chino-stream did not answer: ", "chino-stream.invalid/api/play/i1/info?stream=REDACTED",
			"chino-stream", "GET /api/v1/items/{id}/play/info"},
		{http.MethodGet, "/api/v1/items/i1/poster?stream=" + streamToken(t), nil, "artwork unavailable\n",
			"GET /api/v1/items/{id}/poster: katalog-manager did not answer: ", "katalog-manager.invalid/api/artwork/i1/poster?stream=REDACTED",
			"katalog-manager", "GET /api/v1/items/{id}/poster"},
		{http.MethodPost, "/api/v1/admin/items/i1/package", admin, errPackaging + "\n",
			"POST /api/v1/admin/items/{id}/package: katalog-manager failed: ", "katalog-manager.invalid/api/items/i1/package",
			"katalog-manager", "POST /api/v1/admin/items/{id}/package"},
	} {
		logged.Reset()
		w := do(h, c.method, c.path, c.header)
		if w.Code != http.StatusBadGateway || w.Body.String() != c.want {
			t.Errorf("%s %s: %d %q, want 502 %q", c.method, c.path, w.Code, w.Body, c.want)
		}
		loggedOnce(t, c.method+" "+c.path, logged, c.log, c.url)

		logged.Reset()
		w = httptest.NewRecorder()
		h.ServeHTTP(w, gone(c.method, c.path, c.header))
		if w.Code != http.StatusBadGateway || w.Body.String() != c.want {
			t.Errorf("%s %s, the viewer gone: %d %q, want 502 %q", c.method, c.path, w.Code, w.Body, c.want)
		}
		loggedOnce(t, c.method+" "+c.path+", the viewer gone", logged, c.through+": the viewer went before "+c.upstream+" answered")
	}

	for _, c := range []struct {
		method, path, code string
	}{
		{http.MethodDelete, "/api/v1/me", "account_not_deleted"},
		{http.MethodPost, "/api/v1/notices/read-all", "notices_unavailable"},
	} {
		w := do(h, c.method, c.path, viewer)
		if w.Code != http.StatusBadGateway || answer(t, w)["error"] != c.code || strings.Contains(w.Body.String(), "SQLSTATE") {
			t.Errorf("%s %s: %d %s, want 502 %s and no word of portal-api's", c.method, c.path, w.Code, w.Body, c.code)
		}
	}
}

// An upstream's error answer on the media routes keeps its status and is
// answered a line of chino-api's own, as plain text: the upstream's line for
// 500 or more, the status's words below; of its headers Retry-After and
// Content-Range alone go on, beside chino-api's own CORS. The first bytes of
// an answer of 500 or more are logged, once and quoted; one below 500 is not.
// A capped viewer's Zap feed, which is read whole to be filtered, is
// answered the same.
func TestAnUpstreamsErrorAnswerIsALineOfOurOwn(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range map[string]string{"Server": "upstream/1.0", "X-Internal-Host": "node-7", "Set-Cookie": "s=1",
			"Www-Authenticate": `Bearer realm="media"`, "Content-Type": "text/html", "Retry-After": "30"} {
			w.Header().Set(k, v)
		}
		status := http.StatusInternalServerError
		switch {
		case strings.HasSuffix(r.URL.Path, "/poster"):
			status = http.StatusNotFound
		case strings.HasSuffix(r.URL.Path, ".m4s"):
			w.Header().Set("Content-Range", "bytes */1000")
			status = http.StatusRequestedRangeNotSatisfiable
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "<html>open /media/share/x.mkv: permission denied</html>")
	}))
	t.Cleanup(up.Close)
	is := newIssuer(t)
	h, err := NewRouter(config.Config{
		OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: "http://katalog-api.invalid", StreamBaseURL: up.URL, ArtworkBaseURL: up.URL,
		StreamSigningKey: signingKey,
	}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	logged := logs(t)
	stream := "?stream=" + streamToken(t)
	for _, c := range []struct {
		name, path string
		header     http.Header
		code       int
		body, kept string
		log        string
	}{
		{"play info, 500", "/api/v1/items/i1/play/info" + stream, nil, http.StatusInternalServerError, "playback unavailable\n",
			"Retry-After", "GET /api/v1/items/{id}/play/info: chino-stream answered 500: "},
		{"a poster, 404", "/api/v1/items/i1/poster" + stream, nil, http.StatusNotFound, "Not Found\n", "Retry-After", ""},
		{"a segment, 416", "/api/v1/items/i1/play/v0/seg-00001.m4s" + stream, nil, http.StatusRequestedRangeNotSatisfiable,
			"Requested Range Not Satisfiable\n", "Content-Range", ""},
		{"a capped viewer's Zap feed, 500", "/api/v1/play/zap-feed", bearerWith(t, is, "kid-1", 12), http.StatusInternalServerError,
			"playback unavailable\n", "Retry-After", "GET /api/v1/play/zap-feed: chino-stream answered 500: "},
	} {
		logged.Reset()
		w := do(h, http.MethodGet, c.path, c.header)
		if w.Code != c.code || w.Body.String() != c.body {
			t.Errorf("%s: %d %q, want %d %q", c.name, w.Code, w.Body, c.code, c.body)
		}
		if got := w.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: Content-Type %q, nosniff %q", c.name, got, w.Header().Get("X-Content-Type-Options"))
		}
		for _, k := range []string{"Server", "X-Internal-Host", "Set-Cookie", "Www-Authenticate"} {
			if v := w.Header().Get(k); v != "" {
				t.Errorf("%s: the upstream's %s %q went on", c.name, k, v)
			}
		}
		if w.Header().Get(c.kept) == "" || w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: %s %q, Access-Control-Allow-Origin %q", c.name, c.kept, w.Header().Get(c.kept), w.Header().Get("Access-Control-Allow-Origin"))
		}
		if c.log == "" {
			if n := len(logged.lines()); n != 0 {
				t.Errorf("%s: logged %q, want nothing", c.name, logged)
			}
			continue
		}
		loggedOnce(t, c.name, logged, c.log, `"<html>open /media/share/x.mkv: permission denied</html>"`)
	}
}

// A bug report OpenProject does not take is 502 "bug reports unavailable";
// OpenProject's error is in the log, once, with the route, and not in the
// answer.
func TestABugReportOpenProjectDoesNotTakeAnswersAFixedLine(t *testing.T) {
	is := newIssuer(t)
	tracker := failingUpstream(t)
	h, err := NewRouter(config.Config{
		OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: "http://katalog-api.invalid", StreamBaseURL: "http://chino-stream.invalid",
		ArtworkBaseURL: "http://katalog-manager.invalid", StreamSigningKey: signingKey,
		OpenProjectURL: tracker.URL, OpenProjectToken: "a-token", OpenProjectProjectID: 1, OpenProjectBugTypeID: 1,
	}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	logged := logs(t)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("report", `{"source":"web","kind":"manual","description":"The player froze."}`); err != nil {
		t.Fatal(err)
	}
	_ = form.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/feedback", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.Header.Set("Authorization", "Bearer "+is.token(t, "viewer-1", "chino"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway || w.Body.String() != errFeedback+"\n" {
		t.Errorf("%d %q, want 502 %q", w.Code, w.Body, errFeedback+"\n")
	}
	loggedOnce(t, "the bug report", logged, `POST /api/v1/feedback: OpenProject failed: "openproject: create work package`, "SQLSTATE")
}

// A bearer the verifier refuses is 401 "invalid token", whatever the reason,
// and one made for another audience 401 "invalid token: wrong audience"
// (the word an app reads), told by the first words of the reason; the
// reason is in the log, once, never in the answer.
func TestARefusedBearerIsAnsweredAFixedLine(t *testing.T) {
	is := newIssuer(t)
	h := oidcRouter(t, is) // audience chino
	logged := logs(t)
	good := is.token(t, "viewer-1", "chino")
	for _, c := range []struct {
		name, bearer, want, reason string
	}{
		{"another audience", is.token(t, "viewer-1", "chino-web"), "invalid token: wrong audience\n", "oidc: expected audience "},
		{"another issuer, named as an audience", is.tokenWith(t, "viewer-1", "chino", map[string]any{"iss": "oidc: expected audience x"}),
			"invalid token\n", "different provider"},
		{"expired", is.tokenWith(t, "viewer-1", "chino", map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}),
			"invalid token\n", "expired"},
		{"a signature that does not verify", good[:len(good)-4] + "AAAA", "invalid token\n", "signature"},
		{"no token at all", "not-a-token", "invalid token\n", "malformed"},
	} {
		logged.Reset()
		w := do(h, http.MethodGet, "/api/v1/me", http.Header{"Authorization": {"Bearer " + c.bearer}})
		if w.Code != http.StatusUnauthorized || w.Body.String() != c.want {
			t.Errorf("%s: %d %q, want 401 %q", c.name, w.Code, w.Body, c.want)
		}
		for _, word := range []string{"oidc", "chino-web", is.URL} {
			if strings.Contains(w.Body.String(), word) {
				t.Errorf("%s: the answer says %q: %q", c.name, word, w.Body)
			}
		}
		loggedOnce(t, c.name, logged, "GET /api/v1/me: a bearer was refused: ", c.reason)
	}
	if w := do(h, http.MethodGet, "/api/v1/me", http.Header{"Authorization": {"Bearer " + good}}); w.Code != http.StatusOK {
		t.Errorf("a good bearer: %d %q", w.Code, w.Body)
	}
}
