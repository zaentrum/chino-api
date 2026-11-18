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

// Quote blanks the credentials an upstream's text can carry in other forms
// too, each with a test of its own: the credentials go, the words around
// them stay.
func TestQuoteBlanksEveryFormOfACredential(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"a URL's userinfo", `Get "https://ada:s3cr3t@up.invalid/api/play": EOF`,
			`"Get \"https://REDACTED@up.invalid/api/play\": EOF"`},
		{"Basic", "401 for Authorization Basic YWRhOnMzY3IzdA== on the media share",
			`"401 for Authorization Basic REDACTED on the media share"`},
		{"a short bearer", "refused: Bearer abc123 is unknown", `"refused: Bearer REDACTED is unknown"`},
		{"a credential after #", "redirected to https://up.invalid/cb#access_token=s3cr3t&state=x",
			`"redirected to https://up.invalid/cb#access_token=REDACTED&state=REDACTED"`},
		{"a header quoted as text", "request had Authorization: Digest username=ada, response=s3cr3t\nand more",
			`"request had Authorization: REDACTED\nand more"`},
		{"a header in Go's form", "headers map[Cookie:[session=s3cr3t] X-Account-Deletion-Token:[t0k3n]]",
			`"headers map[Cookie:REDACTED] X-Account-Deletion-Token:REDACTED]]"`},
		{"JSON fields", `{"error":"denied","token":"s3cr3t","password": "hunter2","secret":42,"access_token":"a\"b","user":"ada"}`,
			`"{\"error\":\"denied\",\"token\":\"REDACTED\",\"password\": \"REDACTED\",\"secret\":\"REDACTED\",\"access_token\":\"REDACTED\",\"user\":\"ada\"}"`},
		{"a cookie's value in quotes", `request had Cookie: session="s3cr3t"; theme=dark`, `"request had Cookie: REDACTED"`},
		{"a cookie after one in braces", `answered Set-Cookie: prefs={"t":"x"}; session=s3cr3t`, `"answered Set-Cookie: REDACTED"`},
		{"a quote after the name", `headers {"Authorization": "Bearer s3cr3t", 'Cookie': 'session=s3cr3t'}`,
			`"headers {\"Authorization\": \"REDACTED\", 'Cookie': REDACTED}"`},
		{"headers as JSON fields", `{"X-Account-Deletion-Token":"t0k3n","Set-Cookie":"s=s3cr3t","X-Api-Key":"s3cr3t","Proxy-Authorization":"Basic s3cr3t"}`,
			`"{\"X-Account-Deletion-Token\":\"REDACTED\",\"Set-Cookie\":\"REDACTED\",\"X-Api-Key\":\"REDACTED\",\"Proxy-Authorization\":\"REDACTED\"}"`},
		{"more JSON fields", `{"accessToken":"s3cr3t","refreshToken":"s3cr3t","idToken":"s3cr3t","clientSecret":"s3cr3t","stream_token":"s3cr3t"}`,
			`"{\"accessToken\":\"REDACTED\",\"refreshToken\":\"REDACTED\",\"idToken\":\"REDACTED\",\"clientSecret\":\"REDACTED\",\"stream_token\":\"REDACTED\"}"`},
		{"session and jwt, not code and key", `{"session":"s3cr3t","jwt":"s3cr3t","code":"not_found","key":"i1"}`,
			`"{\"session\":\"REDACTED\",\"jwt\":\"REDACTED\",\"code\":\"not_found\",\"key\":\"i1\"}"`},
		{"a header's token", "refused: Authorization: s3cr3t\nnext", `"refused: Authorization: REDACTED\nnext"`},
		{"a header's scheme and token", "refused Proxy-Authorization: Negotiate s3cr3t", `"refused Proxy-Authorization: REDACTED"`},
		{"the name as HTTP writes it, words after the value", "401: Authorization: Negotiate s3cr3t for the media share\nnext",
			`"401: Authorization: REDACTED\nnext"`},
		{"Proxy-Authorization as HTTP writes it", "sent Proxy-Authorization: s3cr3t was refused", `"sent Proxy-Authorization: REDACTED"`},
		{"name=value", "sent x-api-key=s3cr3t", `"sent x-api-key=REDACTED"`},
		{"name = value", "sent X-Account-Deletion-Token = t0k3n", `"sent X-Account-Deletion-Token = REDACTED"`},
		{"authorization = value", "sent authorization = Bearer s3cr3t", `"sent authorization = REDACTED"`},
		{"cookie=value", "sent cookie=session=s3cr3t", `"sent cookie=REDACTED"`},
		{"a cookie's value with a space", "Cookie: theme=dark mode; session=s3cr3t", `"Cookie: REDACTED"`},
		{"a lower-case cookie line", "cookie: prefs=a b c; session=s3cr3t\nnext", `"cookie: REDACTED\nnext"`},
		{"a ; in quotes", `Set-Cookie: a="x;y"; session=s3cr3t; Path=/`, `"Set-Cookie: REDACTED"`},
		{"words after a value in quotes", `Cookie: a="x" y; session=s3cr3t`, `"Cookie: REDACTED"`},
	} {
		got := Quote(c.in)
		if got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
		for _, secret := range []string{"s3cr3t", "YWRhOnMzY3IzdA", "abc123", "t0k3n", "hunter2", `a\\\"b`} {
			if strings.Contains(got, secret) {
				t.Errorf("%s: %s still carries %s", c.name, got, secret)
			}
		}
	}
}

// A header's name in prose is no header: the words after it stay, but the
// one after "bearer", which Quote takes for a credential wherever it is.
func TestQuoteKeepsTheProseAfterAHeadersName(t *testing.T) {
	for in, want := range map[string]string{
		"missing authorization: the bearer has no admin role": `"missing authorization: the bearer REDACTED no admin role"`,
		"cookie: none sent": `"cookie: none sent"`,
	} {
		if got := Quote(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}
