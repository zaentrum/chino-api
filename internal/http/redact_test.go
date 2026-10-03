package http

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// A bearer as clients put it in ?token= (a JWT) and a stream token.
const (
	urlBearer      = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyLTEifQ.c2lnbmF0dXJlLW9mLXRoZS1iZWFyZXI"
	urlStreamToken = "dXNlci0xfDE3OTEwNjI5NDM.c3RyZWFtLXNpZ25hdHVyZQ"
)

// noSecret fails t when s contains either credential.
func noSecret(t *testing.T, what, s string) {
	t.Helper()
	for _, secret := range []string{urlBearer, "eyJhbGci", urlStreamToken, "c3RyZWFtLXNpZ25hdHVyZQ"} {
		if strings.Contains(s, secret) {
			t.Errorf("%s carries a credential (%s…):\n%s", what, secret[:8], s)
		}
	}
}

// The request log line has the credentials blanked out; the handler still
// gets the real query.
func TestRequestLoggerRedactsCredentials(t *testing.T) {
	var logs bytes.Buffer
	var seen string
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger(&logs))
	r.Get("/api/v1/items/{id}/backdrop", func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("token") + "|" + r.URL.Query().Get("stream")
	})
	uri := "/api/v1/items/i1/backdrop?token=" + urlBearer + "&stream=" + urlStreamToken + "&w=780"
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, uri, nil))

	noSecret(t, "the request log", logs.String())
	if !strings.Contains(logs.String(), `"GET http://example.com/api/v1/items/i1/backdrop?token=REDACTED&stream=REDACTED&w=780 HTTP/1.1"`) {
		t.Errorf("log line %q", logs.String())
	}
	if seen != urlBearer+"|"+urlStreamToken {
		t.Errorf("the handler saw %q", seen)
	}
}

// End to end through NewRouter: the request log on stdout, the telemetry
// lines (players report the URL that failed) and the body of an upstream
// failure keep the credentials out.
func TestRouterKeepsCredentialsOutOfWhatItWrites(t *testing.T) {
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = wr // NewRouter hands its request logger os.Stdout
	h := router(t, "http://katalog-api.invalid", "http://katalog-manager.invalid", false)
	os.Stdout = stdout
	var appLog bytes.Buffer
	log.SetOutput(&appLog)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// The final telemetry flush, as sendBeacon sends it: credential in the
	// URL, a failed media URL and an error text in the payload.
	body := `{"sessionId":"s1","events":[{"ts":1,"kind":"hls_fatal","itemId":"i1","payload":{` +
		`"url":"https://chino.example/api/v1/items/i1/play/audio/0/init.mp4?stream=` + urlStreamToken + `&q=high",` +
		`"reason":"401 for Authorization: Bearer ` + urlBearer + `","httpStatus":500}}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/play/events?token="+urlBearer, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("telemetry: %d %s", w.Code, w.Body)
	}
	// An upstream that cannot be reached: the error names the URL, query
	// included.
	w = do(h, "GET", "/api/v1/items/i1/play/master.m3u8?stream="+urlStreamToken+"&q=high", nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("unreachable upstream: %d %s", w.Code, w.Body)
	}
	noSecret(t, "the upstream error body", w.Body.String())
	if !strings.Contains(w.Body.String(), "master.m3u8?stream=REDACTED&q=high") {
		t.Errorf("upstream error body %q", w.Body)
	}

	_ = wr.Close()
	requestLog, _ := io.ReadAll(rd)
	noSecret(t, "the request log", string(requestLog))
	if !strings.Contains(string(requestLog), "/api/v1/play/events?token=REDACTED") {
		t.Errorf("request log %q", requestLog)
	}
	noSecret(t, "the telemetry log", appLog.String())
	if !strings.Contains(appLog.String(), `init.mp4?stream=REDACTED\u0026q=high`) ||
		!strings.Contains(appLog.String(), `Authorization: Bearer REDACTED`) {
		t.Errorf("telemetry log %q", appLog.String())
	}
}

// Bug reports quote the URLs that failed; what goes to OpenProject does not
// carry their credentials.
func TestFeedbackTicketsCarryNoCredentials(t *testing.T) {
	rep := &feedbackReport{
		Source:      "web",
		Kind:        "player",
		Title:       "fragLoadError on /api/v1/items/i1/play/audio/0/init.mp4?stream=" + urlStreamToken,
		Description: "GET /api/v1/items/i1/backdrop?token=" + urlBearer + " failed\nAuthorization: Bearer " + urlBearer,
		Context:     map[string]string{"url": "/api/v1/items/i1/play/master.m3u8?stream=" + urlStreamToken + "&q=high", "appVersion": "1.2.3 (Bearer " + urlBearer + ")"},
	}
	subject := feedbackSubject(rep)
	noSecret(t, "the subject", subject)
	if !strings.HasPrefix(subject, "[web][player] fragLoadError on ") || !strings.HasSuffix(subject, "?stream=REDACTED") {
		t.Errorf("subject %q", subject)
	}
	desc := feedbackDescription("ada", rep, false)
	noSecret(t, "the description", desc)
	for _, want := range []string{"| url | /api/v1/items/i1/play/master.m3u8?stream=REDACTED&q=high |", "backdrop?token=REDACTED failed", "Bearer REDACTED"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lacks %q:\n%s", want, desc)
		}
	}
	noSecret(t, "the recurrence comment", recurredComment("ada", rep, 3))
}
