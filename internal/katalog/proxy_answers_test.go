package katalog

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// proxyLog is what ProxyStream logs while a test runs, under a lock: a
// server's goroutines write to it.
type proxyLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *proxyLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// lines are the lines logged since the last call, and none of them again.
func (l *proxyLog) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := strings.FieldsFunc(l.b.String(), func(r rune) bool { return r == '\n' })
	l.b.Reset()
	return out
}

func proxyLogs(t *testing.T) *proxyLog {
	t.Helper()
	l := &proxyLog{}
	prev := log.Writer()
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(prev) })
	return l
}

// answering is an upstream that answers every request with status, the
// headers of header and body.
func answering(t *testing.T, status int, header map[string]string, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// proxied is the answer ProxyStream gives a GET of path, chino-stream's or,
// for /api/artwork, katalog-manager's, both at up.
func proxied(up *httptest.Server, path string) *httptest.ResponseRecorder {
	kc := New("http://katalog-api.invalid")
	kc.StreamBaseURL, kc.ArtworkBaseURL = up.URL, up.URL
	w := httptest.NewRecorder()
	kc.ProxyStream(w, httptest.NewRequest(http.MethodGet, "/api/v1/x", nil), path, "")
	return w
}

// leaking are headers of an upstream's that no caller needs.
var leaking = map[string]string{"Server": "upstream/1.0", "X-Internal-Host": "node-7", "Set-Cookie": "s=1",
	"Www-Authenticate": `Bearer realm="media"`, "X-Stream-Mode": "remux", "Access-Control-Allow-Origin": "https://elsewhere.example"}

// An answer below 400 goes on with its body and, of its headers, what a
// player needs alone; with nosniff, and a type a browser opens as a document
// as application/octet-stream.
func TestProxyStreamPassesOnlyWhatAPlayerNeeds(t *testing.T) {
	header := map[string]string{"Content-Type": "text/html; charset=utf-8", "Cache-Control": "public, max-age=60",
		"Etag": `"v1"`, "Last-Modified": "Wed, 21 Oct 2026 07:28:00 GMT", "Accept-Ranges": "bytes", "Vary": "Accept-Encoding"}
	for k, v := range leaking {
		header[k] = v
	}
	const page = "<html>open /media/share/x.mkv: permission denied</html>"
	w := proxied(answering(t, http.StatusOK, header, page), "/api/play/i1/info")
	if w.Code != http.StatusOK || w.Body.String() != page {
		t.Fatalf("%d %q", w.Code, w.Body)
	}
	for k := range leaking {
		if v := w.Header().Get(k); v != "" {
			t.Errorf("the upstream's %s %q went on", k, v)
		}
	}
	for k, want := range map[string]string{"Content-Type": "application/octet-stream", "X-Content-Type-Options": "nosniff",
		"Cache-Control": "public, max-age=60", "Etag": `"v1"`, "Last-Modified": "Wed, 21 Oct 2026 07:28:00 GMT",
		"Accept-Ranges": "bytes", "Vary": "Accept-Encoding", "Content-Length": strconv.Itoa(len(page))} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s %q, want %q", k, got, want)
		}
	}

	segment := proxied(answering(t, http.StatusPartialContent, map[string]string{"Content-Type": "video/iso.segment",
		"Content-Range": "bytes 0-3/1000"}, "abcd"), "/api/play/i1/v0/seg-00001.m4s")
	if segment.Code != http.StatusPartialContent || segment.Header().Get("Content-Type") != "video/iso.segment" ||
		segment.Header().Get("Content-Range") != "bytes 0-3/1000" {
		t.Errorf("a segment's range: %d %v", segment.Code, segment.Header())
	}
}

// Retry-After goes on as seconds or an HTTP date, and Content-Range as
// bytes first-last/length or bytes */length, or not at all.
func TestProxyStreamPassesHeadersOnlyInTheirOwnSyntax(t *testing.T) {
	_ = proxyLogs(t)
	for _, c := range []struct {
		status        int
		header, value string
		kept          bool
	}{
		{http.StatusServiceUnavailable, "Retry-After", "30", true},
		{http.StatusTooManyRequests, "Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT", true},
		{http.StatusServiceUnavailable, "Retry-After", "30; db: connection refused", false},
		{http.StatusServiceUnavailable, "Retry-After", "soon", false},
		{http.StatusRequestedRangeNotSatisfiable, "Content-Range", "bytes */1000", true},
		{http.StatusRequestedRangeNotSatisfiable, "Content-Range", "bytes */1000 open /media/share/x.mkv", false},
		{http.StatusPartialContent, "Content-Range", "bytes 0-99/1000", true},
		{http.StatusPartialContent, "Content-Range", "bytes 0-99/*", true},
		{http.StatusPartialContent, "Content-Range", "items 0-99/1000", false},
	} {
		w := proxied(answering(t, c.status, map[string]string{c.header: c.value}, "x"), "/api/play/i1/v0/seg-00001.m4s")
		got := w.Header().Get(c.header)
		if c.kept && got != c.value || !c.kept && got != "" {
			t.Errorf("%d %s %q: went on as %q, want it kept %v", c.status, c.header, c.value, got, c.kept)
		}
	}
}

// A 503 with a Retry-After is the upstream busy and saying when to ask
// again: answered, not logged. A 503 without one, or with one of no syntax
// of its own, is logged.
func TestProxyStreamDoesNotLogABusyUpstream(t *testing.T) {
	logged := proxyLogs(t)
	for _, c := range []struct {
		retry  string
		logged int
	}{
		{"5", 0},
		{"Wed, 21 Oct 2026 07:28:00 GMT", 0},
		{"", 1},
		{"soon", 1},
	} {
		header := map[string]string{}
		if c.retry != "" {
			header["Retry-After"] = c.retry
		}
		w := proxied(answering(t, http.StatusServiceUnavailable, header, "warming up"), "/api/play/i1/info")
		if w.Code != http.StatusServiceUnavailable || w.Body.String() != "playback unavailable\n" {
			t.Errorf("Retry-After %q: %d %q", c.retry, w.Code, w.Body)
		}
		if lines := logged.lines(); len(lines) != c.logged {
			t.Errorf("Retry-After %q: logged %q, want %d lines", c.retry, lines, c.logged)
		}
	}
}

// breaking is an upstream that begins an answer of 200 and breaks off its
// connection after body: with a length it does not reach, or chunked.
func breaking(t *testing.T, length bool, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		const head = "HTTP/1.1 200 OK\r\nContent-Type: video/mp4\r\nCache-Control: public, max-age=31536000\r\n"
		if length {
			_, _ = buf.WriteString(head + "Content-Length: 1000\r\n\r\n" + body)
		} else {
			_, _ = buf.WriteString(head + "Transfer-Encoding: chunked\r\n\r\n")
			if body != "" {
				_, _ = fmt.Fprintf(buf, "%x\r\n%s\r\n", len(body), body)
			}
		}
		_ = buf.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// An upstream that breaks off its answer once bytes of it are out is
// logged, and the caller's answer is cut off too: never ended as if it were
// whole.
func TestProxyStreamCutsOffAnAnswerTheUpstreamBreaksOff(t *testing.T) {
	logged := proxyLogs(t)
	for _, length := range []bool{true, false} {
		kc := New("http://katalog-api.invalid")
		kc.StreamBaseURL = breaking(t, length, "hello").URL
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			kc.ProxyStream(w, r, "/api/play/i1/v0/seg-00001.m4s", "")
		}))
		// Cut off: no answer at all (the bytes before the break not sent yet),
		// or one whose body ends in an error.
		resp, err := http.Get(api.URL + "/api/v1/items/i1/play/v0/seg-00001.m4s")
		if err == nil {
			var body []byte
			body, err = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err == nil {
				t.Errorf("with a length %v: %d %q read whole, want it cut off", length, resp.StatusCode, body)
			}
		}
		api.Close()
		lines := logged.lines()
		if len(lines) != 1 || !strings.Contains(lines[0], "chino-stream broke off its answer after 5 bytes: ") {
			t.Errorf("with a length %v: logged %q", length, lines)
		}
	}
}

// An upstream that breaks off its answer before its first bytes, nothing of
// it out yet, is answered 502 with the upstream's line and none of its
// headers, and logged.
func TestProxyStreamAnswers502ForAnAnswerBrokenOffBeforeItsFirstBytes(t *testing.T) {
	logged := proxyLogs(t)
	for _, length := range []bool{true, false} {
		w := proxied(breaking(t, length, ""), "/api/play/i1/v0/seg-00001.m4s")
		if w.Code != http.StatusBadGateway || w.Body.String() != "playback unavailable\n" ||
			w.Header().Get("Content-Type") != "text/plain; charset=utf-8" || w.Header().Get("Cache-Control") != "" {
			t.Errorf("with a length %v: %d %q %v, want 502 and the line alone", length, w.Code, w.Body, w.Header())
		}
		lines := logged.lines()
		if len(lines) != 1 || !strings.Contains(lines[0], "chino-stream broke off its answer after 0 bytes: ") {
			t.Errorf("with a length %v: logged %q", length, lines)
		}
	}
}

// A viewer who leaves mid-answer, bytes of it out, is not logged: the
// upstream's answer is broken off by the viewer's going, not the upstream.
func TestProxyStreamDoesNotLogAViewerWhoLeavesMidAnswer(t *testing.T) {
	logged := proxyLogs(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/iso.segment")
		_, _ = io.WriteString(w, "hello")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // the rest never comes
	}))
	t.Cleanup(up.Close)
	kc := New("http://katalog-api.invalid")
	kc.StreamBaseURL = up.URL
	wrote, done := make(chan struct{}), make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		kc.ProxyStream(&firstWrite{ResponseWriter: w, wrote: wrote}, r, "/api/play/i1/v0/seg-00001.m4s", "")
	}))
	t.Cleanup(api.Close)

	conn, err := net.Dial("tcp", api.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "GET /api/v1/items/i1/play/v0/seg-00001.m4s HTTP/1.1\r\nHost: example.com\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first bytes written", wrote)
	_ = conn.Close() // the viewer leaves
	waitFor(t, "the handler's end", done)
	if lines := logged.lines(); len(lines) != 0 {
		t.Errorf("logged %q, want nothing", lines)
	}
}

// waitFor fails t unless c is closed within 10 seconds.
func waitFor(t *testing.T, what string, c chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: not in time", what)
	}
}

// firstWrite is a ResponseWriter that closes wrote at its first Write.
type firstWrite struct {
	http.ResponseWriter
	wrote chan struct{}
	once  sync.Once
}

func (f *firstWrite) Write(p []byte) (int, error) {
	n, err := f.ResponseWriter.Write(p)
	f.once.Do(func() { close(f.wrote) })
	return n, err
}

// An answer below 400 goes on with a type the routes serve, as it is
// (servedTypes), and of any other type as application/octet-stream: the
// +xml family, XSL, scripts by any of their names, a page, multipart's
// replacing answer, a type sent twice, and an answer with no type at all,
// which the response writer would take from its body. Its parameters are
// kept only as a charset of utf-8 and a byteranges' boundary.
func TestProxyStreamPassesOnlyTheTypesTheRoutesServe(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = r.URL.Query()["type"] // none where nil: no type, not one sniffed
		_, _ = io.WriteString(w, "<html><script>x()</script></html>")
	}))
	t.Cleanup(up.Close)
	kc := New("http://katalog-api.invalid")
	kc.StreamBaseURL, kc.ArtworkBaseURL = up.URL, up.URL
	served := func(types ...string) string {
		q := url.Values{"type": types}
		w := httptest.NewRecorder()
		kc.ProxyStream(w, httptest.NewRequest(http.MethodGet, "/api/v1/x?"+q.Encode(), nil), "/api/play/i1/info", "")
		if w.Code != http.StatusOK {
			t.Errorf("%q: %d", types, w.Code)
		}
		return strings.Join(w.Header().Values("Content-Type"), ", ")
	}
	const opaque = "application/octet-stream"
	for _, typ := range []string{
		"application/rss+xml", "application/atom+xml", "application/xslt+xml", "application/mathml+xml",
		"application/rdf+xml", "application/vnd.example+xml", "image/svg+xml", "application/xhtml+xml",
		"video/example+xml", "text/xml", "application/xml", "text/xsl",
		"text/javascript", "application/javascript", "application/ecmascript", "text/ecmascript",
		"application/x-javascript", "text/x-javascript", "application/x-ecmascript", "text/jscript",
		"text/livescript", "text/javascript1.5", "text/html", "text/html; charset=utf-8", "image/svg",
		"multipart/x-mixed-replace; boundary=next", "text/css", "application/pdf", "",
		"not a type",
	} {
		if got := served(typ); got != opaque {
			t.Errorf("%q went on as %q, want %s", typ, got, opaque)
		}
	}
	if got := served(); got != opaque {
		t.Errorf("no type went on as %q, want %s", got, opaque)
	}
	if got := served("video/mp4", "text/html"); got != opaque {
		t.Errorf("two types went on as %q, want %s", got, opaque)
	}
	for typ, want := range map[string]string{
		"video/iso.segment": "video/iso.segment", "video/mp4": "video/mp4", "video/x-matroska": "video/x-matroska",
		"audio/mp4": "audio/mp4", "audio/aac": "audio/aac", "image/jpeg": "image/jpeg", "image/png": "image/png",
		"image/webp": "image/webp", "font/ttf": "font/ttf", "font/woff2": "font/woff2",
		"application/vnd.apple.mpegurl": "application/vnd.apple.mpegurl", "application/x-mpegURL": "application/x-mpegurl",
		"text/vtt; charset=utf-8": "text/vtt; charset=utf-8", "text/vtt; charset=UTF-8": "text/vtt; charset=utf-8",
		"text/vtt": "text/vtt", "text/x-ssa; charset=utf-8": "text/x-ssa; charset=utf-8",
		"application/pgs": "application/pgs", "application/x-vobsub": "application/x-vobsub",
		"application/dvb-subtitles": "application/dvb-subtitles", "text/plain; charset=utf-8": "text/plain; charset=utf-8",
		"application/json": "application/json", "application/octet-stream": opaque,
		"multipart/byteranges; boundary=3d6b6a416f9b5": "multipart/byteranges; boundary=3d6b6a416f9b5",
		"Video/MP4":                                "video/mp4",
		`video/mp4; codecs="avc1.64001f"`:          "video/mp4",
		"text/plain; charset=utf-7":                "text/plain",
		`application/json; charset=utf-8; x="<b>"`: "application/json; charset=utf-8",
		"image/png; boundary=x":                    "image/png",
	} {
		if got := served(typ); got != want {
			t.Errorf("%q went on as %q, want %q", typ, got, want)
		}
	}
}

// An answer that carries no body keeps having no type: a 304, a 204.
func TestProxyStreamGivesNoTypeToAnAnswerWithoutABody(t *testing.T) {
	for _, status := range []int{http.StatusNotModified, http.StatusNoContent} {
		w := proxied(answering(t, status, map[string]string{"Etag": `"v1"`}, ""), "/api/artwork/i1/poster")
		if w.Code != status || w.Header().Get("Content-Type") != "" || w.Header().Get("Etag") != `"v1"` {
			t.Errorf("%d: %d %v", status, w.Code, w.Header())
		}
	}
}

// An upstream's redirect is not followed, by the stream client or the
// catalog's: no request reaches its target, another port of the same host,
// the bearer with it; the media proxy answers the caller the upstream's 3xx
// without its Location, and a call of the catalog's fails.
func TestAnUpstreamsRedirectIsNotFollowed(t *testing.T) {
	_ = proxyLogs(t)
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		_, _ = io.WriteString(w, "the target's answer")
	}))
	t.Cleanup(target.Close)
	up := answering(t, http.StatusFound, map[string]string{"Location": target.URL + "/elsewhere", "Content-Type": "text/plain"}, "")

	kc := New(up.URL)
	kc.StreamBaseURL, kc.ArtworkBaseURL = up.URL, up.URL
	for _, path := range []string{"/api/play/i1/info", "/api/artwork/i1/poster"} {
		w := httptest.NewRecorder()
		kc.ProxyStream(w, httptest.NewRequest(http.MethodGet, "/api/v1/x", nil), path, "a-bearer")
		if w.Code != http.StatusFound || w.Header().Get("Location") != "" || strings.Contains(w.Body.String(), "target") {
			t.Errorf("%s: %d, Location %q, %q", path, w.Code, w.Header().Get("Location"), w.Body)
		}
	}
	if _, err := kc.GetItem(context.Background(), "a-bearer", "i1"); err == nil {
		t.Error("a call of the catalog's took the redirect for an answer")
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("the redirect's target was asked %d times", n)
	}
}
