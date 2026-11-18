package http

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
	"github.com/zaentrum/chino-api/internal/katalog"
	"github.com/zaentrum/chino-api/internal/store"
)

// breakingText is an upstream's error text with a line break in it, before
// words in the form of a line of chino-api's, and longer than the bound of
// what a line carries of it: the line that quotes it stays one line, the
// break written as \n, and is cut at the bound.
var breakingText = "boom\nchino-api: GET /admin: all is well " + strings.Repeat("x", 600)

// breakingUpstream fails every request with 500 and breakingText.
func breakingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, breakingText, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// bounded fails t unless each line logged is shorter than 1 KB.
func bounded(t *testing.T, what string, logged *logBuffer) {
	t.Helper()
	for _, l := range logged.lines() {
		if len(l) > 1024 {
			t.Errorf("%s: a line of %d bytes", what, len(l))
		}
	}
}

// The log lines of the episodes of a file, the subtitle languages, the
// notices, account deletion and bug reports: one bounded line each, the
// upstream's error quoted (its line break written as \n) and the route; a
// viewer gone before the answer logged in the viewer's words.
func TestTheOtherLogLinesAreBoundedAndSayWhenTheViewerWent(t *testing.T) {
	logged := logs(t)
	up := breakingUpstream(t)
	const escaped = `boom\nchino-api: GET /admin: all is well`

	mux := chi.NewRouter()
	kc := katalog.New(up.URL)
	mux.Post("/api/v1/items/{id}/progress", func(w http.ResponseWriter, r *http.Request) {
		_ = fileOf(r, &store.Store{}, kc, "viewer-1", chi.URLParam(r, "id"))
	})
	mux.Get(languagesRoute, newSubtitleLanguages(up.URL).ServeHTTP)
	for _, c := range []struct {
		name, method, path, failed, went string
	}{
		{"the episodes of a file", http.MethodPost, "/api/v1/items/e1/progress",
			`POST /api/v1/items/{id}/progress: katalog-api failed (the episodes of the title's file, written for the title alone): "`,
			"POST /api/v1/items/{id}/progress: the viewer went before katalog-api answered (the episodes of the title's file, written for the title alone)"},
		{"the subtitle languages", http.MethodGet, languagesRoute,
			`GET /api/v1/subtitles/languages: katalog-manager failed (the subtitle languages, none kept young enough to serve): "`,
			"GET /api/v1/subtitles/languages: the viewer went before katalog-manager answered (the subtitle languages, none kept young enough to serve)"},
	} {
		logged.Reset()
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
		loggedOnce(t, c.name, logged, c.failed, escaped)
		bounded(t, c.name, logged)
		logged.Reset()
		mux.ServeHTTP(httptest.NewRecorder(), gone(c.method, c.path, nil))
		loggedOnce(t, c.name+", the viewer gone", logged, c.went)
	}

	is := newIssuer(t)
	h := accountRouter(t, is, up.URL, deletionToken, nil)
	viewer := bearerWith(t, is, "viewer-1", nil)
	logged.Reset()
	if w := do(h, http.MethodPost, "/api/v1/notices/read-all", viewer); w.Code != http.StatusBadGateway {
		t.Errorf("a notice's change: %d %q", w.Code, w.Body)
	}
	loggedOnce(t, "a notice's change", logged, `POST /api/v1/notices/read-all: portal-api failed (a notice's change): "`)
	logged.Reset()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, gone(http.MethodPost, "/api/v1/notices/read-all", viewer))
	if w.Code != http.StatusBadGateway {
		t.Errorf("a notice's change, the viewer gone: %d", w.Code)
	}
	loggedOnce(t, "a notice's change, the viewer gone", logged,
		"POST /api/v1/notices/read-all: the viewer went before portal-api answered (a notice's change)")

	logged.Reset()
	if w := do(h, http.MethodDelete, "/api/v1/me", viewer); w.Code != http.StatusBadGateway {
		t.Errorf("an account's deletion: %d %q", w.Code, w.Body)
	}
	if lines := logged.lines(); len(lines) != 2 ||
		!strings.Contains(lines[0], `DELETE /api/v1/me: account deletion: portal-api gave no answer, or a 5xx; asked once more: "`) ||
		!strings.Contains(lines[1], `DELETE /api/v1/me: account deletion: portal-api did not delete the account: "`) {
		t.Errorf("an account's deletion: logged %q", lines)
	}

	// A bug report filed, its screenshot not taken.
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/attachments") {
			http.Error(w, breakingText, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7})
	}))
	t.Cleanup(tracker.Close)
	fh, err := NewRouter(config.Config{
		OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: "http://katalog-api.invalid", StreamBaseURL: "http://chino-stream.invalid",
		ArtworkBaseURL: "http://katalog-manager.invalid", StreamSigningKey: signingKey,
		OpenProjectURL: tracker.URL, OpenProjectToken: "a-token", OpenProjectProjectID: 1, OpenProjectBugTypeID: 1,
	}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("report", `{"source":"web","kind":"manual","description":"The player froze."}`)
	part, _ := form.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="screenshot"; filename="s.png"`},
		"Content-Type": {"image/png"}})
	_, _ = io.WriteString(part, "\x89PNG")
	_ = form.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/feedback", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.Header["Authorization"] = viewer["Authorization"]
	logged.Reset()
	w = httptest.NewRecorder()
	fh.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Errorf("a bug report: %d %q", w.Code, w.Body)
	}
	loggedOnce(t, "a bug report's screenshot", logged,
		`POST /api/v1/feedback: OpenProject failed (the screenshot of work package 7, filed without it): "`, escaped)
	bounded(t, "a bug report's screenshot", logged)
}

// Against Postgres: an account's deletion whose database fails is logged as
// the database's failure, at ERROR (slog's default handler writes the level
// before the line), and as the viewer having gone where it went.
func TestAnAccountsDeletionLogsAViewerGoneInItsWords(t *testing.T) {
	st := testStore(t)
	is := newIssuer(t)
	fp := newFakePortal(t)
	h := accountRouter(t, is, fp.URL, deletionToken, st)
	logged := logs(t)
	viewer := bearerWith(t, is, "viewer-1", nil)
	admin := http.Header{"Authorization": {"Bearer " + is.tokenWith(t, "admin-1", "chino", realm("zaentrum-admin"))},
		"X-Account-Deletion-Token": {deletionToken}}
	if w := do(h, http.MethodGet, "/api/v1/me", viewer); w.Code != http.StatusOK { // the issuer's keys, asked once
		t.Fatalf("GET /me: %d", w.Code)
	}
	st.Close()
	for _, c := range []struct {
		name, path   string
		header       http.Header
		failed, went string
	}{
		{"the person's own", "/api/v1/me", viewer, `ERROR chino-api: DELETE /api/v1/me: the database failed (the account's deletion): "`,
			"DELETE /api/v1/me: the viewer went before the database answered (the account's deletion)"},
		{"an admin's", "/api/v1/admin/accounts/viewer-2/data", admin,
			`ERROR chino-api: DELETE /api/v1/admin/accounts/{sub}/data: the database failed (an account's data, for portal-api): "`,
			"DELETE /api/v1/admin/accounts/{sub}/data: the viewer went before the database answered (an account's data, for portal-api)"},
	} {
		logged.Reset()
		if w := do(h, http.MethodDelete, c.path, c.header); w.Code != http.StatusInternalServerError {
			t.Errorf("%s: %d %q", c.name, w.Code, w.Body)
		}
		loggedOnce(t, c.name, logged, c.failed)
		logged.Reset()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, gone(http.MethodDelete, c.path, c.header))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("%s, the viewer gone: %d %q", c.name, w.Code, w.Body)
		}
		loggedOnce(t, c.name+", the viewer gone", logged, c.went)
	}
}

// A capped viewer's Zap feed or packaged ids that are not their list are
// chino-stream's failure: 502 "playback unavailable", logged as
// chino-stream's, with nothing asked of the catalog.
func TestACappedViewersListThatIsNotOneIsChinoStreams(t *testing.T) {
	stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "<html>not a list</html>")
	}))
	t.Cleanup(stream.Close)
	is := newIssuer(t)
	h, err := NewRouter(config.Config{
		OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: failingUpstream(t).URL, StreamBaseURL: stream.URL,
		ArtworkBaseURL: "http://katalog-manager.invalid", StreamSigningKey: signingKey,
	}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	logged := logs(t)
	kid := bearerWith(t, is, "kid-1", 12)
	for _, path := range []string{"/api/v1/play/zap-feed", "/api/v1/play/packaged-ids"} {
		logged.Reset()
		if w := do(h, http.MethodGet, path, kid); w.Code != http.StatusBadGateway || w.Body.String() != errPlayback+"\n" {
			t.Errorf("%s: %d %q, want 502 %q", path, w.Code, w.Body, errPlayback+"\n")
		}
		loggedOnce(t, path, logged, "GET "+path+`: chino-stream failed: "chino-stream's answer is not its list`)
		if strings.Contains(logged.String(), "katalog-api") {
			t.Errorf("%s: logged as the catalog's: %q", path, logged)
		}
	}
}

// GET /api/config's apiBase is the request's own only where its Host is a
// host in its own syntax, its port of 1 to 65535, and its X-Forwarded-Proto
// http or https; "/api" for a Host of any other form. Where PUBLIC_BASE_URL is unset the answer is
// kept by the caller's cache alone; with it, by any.
func TestTheConfigsAPIBaseIsTheRequestsOnlyAsAHost(t *testing.T) {
	for _, c := range []struct {
		public, host, proto, base, cache string
	}{
		{"", "chino.example:8443", "", "http://chino.example:8443/api", "private, max-age=300"},
		{"", "chino.example", "https", "https://chino.example/api", "private, max-age=300"},
		{"", "chino.example", "HTTPS, http", "https://chino.example/api", "private, max-age=300"},
		{"", "chino.example", "javascript", "http://chino.example/api", "private, max-age=300"},
		{"", "[2001:db8::1]:8080", "", "http://[2001:db8::1]:8080/api", "private, max-age=300"},
		{"", "192.0.2.10", "", "http://192.0.2.10/api", "private, max-age=300"},
		{"", "elsewhere.example/sign-in", "https", "/api", "private, max-age=300"},
		{"", "ada@elsewhere.example", "https", "/api", "private, max-age=300"},
		{"", "elsewhere.example:80:80", "", "/api", "private, max-age=300"},
		{"", "elsewhere.example?x=1", "", "/api", "private, max-age=300"},
		{"", "chino.example:1", "", "http://chino.example:1/api", "private, max-age=300"},
		{"", "chino.example:65535", "", "http://chino.example:65535/api", "private, max-age=300"},
		{"", "chino.example:0", "", "/api", "private, max-age=300"},
		{"", "chino.example:65536", "", "/api", "private, max-age=300"},
		{"", "chino.example:99999", "", "/api", "private, max-age=300"},
		{"", "[2001:db8::1]:70000", "", "/api", "private, max-age=300"},
		{"", "", "", "/api", "private, max-age=300"},
		{"https://chino.example/", "elsewhere.example/sign-in", "http", "https://chino.example/api", "public, max-age=300"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/config", nil)
		r.Host = c.host
		if c.proto != "" {
			r.Header.Set("X-Forwarded-Proto", c.proto)
		}
		w := httptest.NewRecorder()
		appConfig(config.Config{PublicBaseURL: c.public}, false)(w, r)
		var doc struct {
			APIBase string `json:"apiBase"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.APIBase != c.base || w.Header().Get("Cache-Control") != c.cache {
			t.Errorf("Host %q, X-Forwarded-Proto %q, PUBLIC_BASE_URL %q: apiBase %q, Cache-Control %q; want %q, %q",
				c.host, c.proto, c.public, doc.APIBase, w.Header().Get("Cache-Control"), c.base, c.cache)
		}
	}
}

// breakingStream is a chino-stream that begins every answer of 200 with body
// and breaks off its connection there.
func breakingStream(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n" + body)
		_ = buf.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A capped viewer's Zap feed or packaged ids that chino-stream breaks off,
// after its first bytes or before them, is 502 "playback unavailable": the
// answer is held to be filtered, and nothing of it has gone out. The break
// is logged once, as chino-stream's.
func TestACappedViewersListBrokenOffIs502(t *testing.T) {
	is := newIssuer(t)
	kid := bearerWith(t, is, "kid-1", 12)
	logged := logs(t)
	for _, body := range []string{`{"items":[`, ""} {
		h, err := NewRouter(config.Config{
			OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
			KatalogBaseURL: failingUpstream(t).URL, StreamBaseURL: breakingStream(t, body).URL,
			ArtworkBaseURL: "http://katalog-manager.invalid", StreamSigningKey: signingKey,
		}, nil, eventsse.NewBroker())
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/api/v1/play/zap-feed", "/api/v1/play/packaged-ids"} {
			logged.Reset()
			if w := do(h, http.MethodGet, path, kid); w.Code != http.StatusBadGateway || w.Body.String() != errPlayback+"\n" {
				t.Errorf("%s, broken off after %q: %d %q, want 502 %q", path, body, w.Code, w.Body, errPlayback+"\n")
			}
			loggedOnce(t, path, logged, "GET "+path+": chino-stream broke off its answer after "+strconv.Itoa(len(body))+" bytes: ")
		}
	}
}

// levelled is what chino-api logs while the test runs as main's JSON handler
// writes it, log's lines among them: one object a line, with its level.
func levelled(t *testing.T) *logBuffer {
	t.Helper()
	b := &logBuffer{}
	prev, out, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(out)
		log.SetFlags(flags)
	})
	return b
}

// loggedAt fails t unless one line logged holds msg, and it is at level.
func loggedAt(t *testing.T, logged *logBuffer, level, msg string) {
	t.Helper()
	var at []string
	for _, l := range logged.lines() {
		var line struct{ Level, Msg string }
		if err := json.Unmarshal([]byte(l), &line); err != nil {
			t.Errorf("a line that is no JSON object: %q", l)
			continue
		}
		if strings.Contains(line.Msg, msg) {
			at = append(at, line.Level)
		}
	}
	if len(at) != 1 || at[0] != level {
		t.Errorf("%q: logged at %q, want once at %s", msg, at, level)
	}
}

// The lines of a notice's change, account deletion, the subtitle languages
// and a bug report's screenshot keep their level, as main's JSON handler
// writes it: WARN, and ERROR for account deletion's but portal-api asked
// once more.
func TestTheFailuresKeepTheirLevel(t *testing.T) {
	up := failingUpstream(t)
	is := newIssuer(t)
	h := accountRouter(t, is, up.URL, deletionToken, nil)
	viewer := bearerWith(t, is, "viewer-1", nil)
	logged := levelled(t)

	do(h, http.MethodPost, "/api/v1/notices/read-all", viewer)
	loggedAt(t, logged, "WARN", "POST /api/v1/notices/read-all: portal-api failed (a notice's change): ")
	logged.Reset()
	do(h, http.MethodDelete, "/api/v1/me", viewer)
	loggedAt(t, logged, "WARN", "DELETE /api/v1/me: account deletion: portal-api gave no answer, or a 5xx; asked once more: ")
	loggedAt(t, logged, "ERROR", "DELETE /api/v1/me: account deletion: portal-api did not delete the account: ")

	mux := chi.NewRouter()
	none, last := newSubtitleLanguages(up.URL), newSubtitleLanguages(up.URL)
	last.keep([]string{"en"}, time.Now().Add(-10*time.Minute))
	mux.Get("/none", none.ServeHTTP)
	mux.Get("/last", last.ServeHTTP)
	logged.Reset()
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/none", nil))
	loggedAt(t, logged, "WARN", "GET /none: katalog-manager failed (the subtitle languages, none kept young enough to serve): ")
	logged.Reset()
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/last", nil))
	loggedAt(t, logged, "WARN", "GET /last: katalog-manager failed (the subtitle languages, the last answer served, 10m0s old): ")

	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/attachments") {
			http.Error(w, upstreamText, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7})
	}))
	t.Cleanup(tracker.Close)
	fh, err := NewRouter(config.Config{
		OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: "http://katalog-api.invalid", StreamBaseURL: "http://chino-stream.invalid",
		ArtworkBaseURL: "http://katalog-manager.invalid", StreamSigningKey: signingKey,
		OpenProjectURL: tracker.URL, OpenProjectToken: "a-token", OpenProjectProjectID: 1, OpenProjectBugTypeID: 1,
	}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("report", `{"source":"web","kind":"manual","description":"The player froze."}`)
	part, _ := form.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="screenshot"; filename="s.png"`},
		"Content-Type": {"image/png"}})
	_, _ = io.WriteString(part, "\x89PNG")
	_ = form.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/feedback", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.Header["Authorization"] = viewer["Authorization"]
	logged.Reset()
	fh.ServeHTTP(httptest.NewRecorder(), r)
	loggedAt(t, logged, "WARN", "POST /api/v1/feedback: OpenProject failed (the screenshot of work package 7, filed without it): ")
}

// No upstream's redirect is followed: portal-api's (the notices, account
// deletion, the extension slots), OpenProject's (a bug report),
// katalog-manager's (the subtitle languages, an admin's packaging) and
// chino-stream's. No request reaches the redirect's target, another port of
// the same host, the bearer and the tokens with it; the media proxy and an
// admin's packaging answer the redirect's status without its Location.
func TestNoUpstreamsRedirectIsFollowed(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		_, _ = io.WriteString(w, `{"id":1,"languages":["en"]}`)
	}))
	t.Cleanup(target.Close)
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirecting.Close)
	is := newIssuer(t)
	h, err := NewRouter(config.Config{
		OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: "http://katalog-api.invalid", StreamBaseURL: redirecting.URL, ArtworkBaseURL: redirecting.URL,
		KatalogManagerURL: redirecting.URL, PortalBaseURL: redirecting.URL, AccountDeletionToken: deletionToken,
		OpenProjectURL: redirecting.URL, OpenProjectToken: "a-token", OpenProjectProjectID: 1, OpenProjectBugTypeID: 1,
		StreamSigningKey: signingKey,
	}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	_ = logs(t)
	viewer := bearerWith(t, is, "viewer-1", nil)
	admin := http.Header{"Authorization": {"Bearer " + is.tokenWith(t, "admin-1", "chino", realm("zaentrum-admin"))}}
	for _, c := range []struct {
		method, path string
		header       http.Header
		code         int
	}{
		{http.MethodGet, "/api/v1/notices", viewer, http.StatusOK},
		{http.MethodPost, "/api/v1/notices/read-all", viewer, http.StatusBadGateway},
		{http.MethodGet, "/api/v1/extensions?slot=search.empty", viewer, http.StatusOK},
		{http.MethodDelete, "/api/v1/me", viewer, http.StatusBadGateway},
		{http.MethodGet, "/api/v1/subtitles/languages", viewer, http.StatusBadGateway},
		{http.MethodPost, "/api/v1/admin/items/i1/package", admin, http.StatusTemporaryRedirect},
		{http.MethodGet, "/api/v1/items/i1/play/info?stream=" + streamToken(t), nil, http.StatusTemporaryRedirect},
		{http.MethodGet, "/api/v1/items/i1/poster?stream=" + streamToken(t), nil, http.StatusTemporaryRedirect},
	} {
		w := do(h, c.method, c.path, c.header)
		if w.Code != c.code {
			t.Errorf("%s %s: %d %q, want %d", c.method, c.path, w.Code, w.Body, c.code)
		}
		if c.code/100 == 3 && w.Header().Get("Location") != "" {
			t.Errorf("%s %s: Location %q went on", c.method, c.path, w.Header().Get("Location"))
		}
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("report", `{"source":"web","kind":"manual","description":"The player froze."}`)
	_ = form.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/feedback", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.Header["Authorization"] = viewer["Authorization"]
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway || w.Body.String() != errFeedback+"\n" {
		t.Errorf("a bug report: %d %q, want 502 %q", w.Code, w.Body, errFeedback+"\n")
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("the redirects' target was asked %d times", n)
	}
}

// A capped viewer's Zap feed or packaged ids longer than maxHeld are not
// held: 502 "playback unavailable", logged once as chino-stream's. One just
// under the bound is filtered as any other, the rest of it as it is.
func TestACappedViewersListOverTheBoundIs502(t *testing.T) {
	var pad atomic.Int64
	stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ids":["adult","kid"],"items":[],"pad":"`+strings.Repeat("x", int(pad.Load()))+`"}`)
	}))
	t.Cleanup(stream.Close)
	is := newIssuer(t)
	h, err := NewRouter(config.Config{
		OIDCIssuer: is.URL, OIDCAudience: "chino", OIDCEnabled: true,
		KatalogBaseURL: ratedKatalog(t).URL, StreamBaseURL: stream.URL,
		ArtworkBaseURL: "http://katalog-manager.invalid", StreamSigningKey: signingKey,
	}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	logged := logs(t)
	kid := bearerWith(t, is, "kid-1", 12)

	pad.Store(maxHeld)
	for _, path := range []string{"/api/v1/play/zap-feed", "/api/v1/play/packaged-ids"} {
		logged.Reset()
		if w := do(h, http.MethodGet, path, kid); w.Code != http.StatusBadGateway || w.Body.String() != errPlayback+"\n" {
			t.Errorf("%s over the bound: %d, %d bytes, want 502 %q", path, w.Code, w.Body.Len(), errPlayback+"\n")
		}
		loggedOnce(t, path, logged, "GET "+path+`: chino-stream failed: "chino-stream's answer is longer than the 8 MiB held of it"`)
	}

	pad.Store(maxHeld - 100)
	w := do(h, http.MethodGet, "/api/v1/play/packaged-ids", kid)
	var list struct {
		IDs []string `json:"ids"`
		Pad string   `json:"pad"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil ||
		strings.Join(list.IDs, ",") != "kid" || len(list.Pad) != maxHeld-100 {
		t.Errorf("under the bound: %d, ids %q, a pad of %d bytes; want 200, kid, %d", w.Code, list.IDs, len(list.Pad), maxHeld-100)
	}
}
