package logline

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Quote keeps what it quotes on one line, at most Max bytes of it, cut at a
// character, without the credentials and the query values in it.
func TestQuoteIsOneBoundedValue(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"a line break", "boom\nchino-api: forged", `"boom\nchino-api: forged"`},
		{"control characters", "a\x00b\rc\x1b[31m", `"a\x00b\rc\x1b[31m"`},
		{"credentials", `Get "http://up.invalid/a?stream=s3cr3t&token=t0k": Bearer abcdefghijklmnopqrstuvwx`,
			`"Get \"http://up.invalid/a?stream=REDACTED&token=REDACTED\": Bearer REDACTED"`},
		{"query values", `Get "http://up.invalid/api/v1/search?limit=30&q=secret+words": no such host`,
			`"Get \"http://up.invalid/api/v1/search?limit=REDACTED&q=REDACTED\": no such host"`},
		{"short", "katalog genres: 500", `"katalog genres: 500"`},
	} {
		if got := Quote(c.in); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}

	long := strings.Repeat("é", Max) // 2 bytes each
	got, err := strconv.Unquote(Quote(long))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Repeat("é", Max/2) + "…"; got != want {
		t.Errorf("a long value: %d bytes %q, want the first %d bytes and …", len(got), got[:8], Max)
	}
	if got := Quote(strings.Repeat("\n", 100*Max)); len(got) > 2*Max+8 {
		t.Errorf("a long value of line breaks: %d bytes", len(got))
	}
}

// Route is the route pattern a request was routed by, else its path, quoted.
func TestRouteIsThePatternElseThePathQuoted(t *testing.T) {
	var routed string
	mux := chi.NewRouter()
	mux.Get("/api/v1/items/{id}", func(_ http.ResponseWriter, r *http.Request) { routed = Route(r) })
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/items/i1%0Aforged", nil))
	if routed != "/api/v1/items/{id}" {
		t.Errorf("routed: %q", routed)
	}
	if got := Route(httptest.NewRequest(http.MethodGet, "/api/v1/x%0Aforged", nil)); got != `"/api/v1/x\nforged"` {
		t.Errorf("not routed: %s", got)
	}
}
