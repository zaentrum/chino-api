// Package logline is what chino-api's own log lines about a request carry
// of it: the route it was routed by, and each value that came from a caller
// or an upstream (a path, a reason, a URL, the first bytes of an answer)
// through Quote, so that every such line stays one line of a bounded length.
package logline

import (
	"net/http"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/redact"
)

// Max is the most bytes of a value a line carries.
const Max = 256

// queryValue matches the value of a query's pair in the text: a URL's, a
// request URI's, a bare query's.
var queryValue = regexp.MustCompile(`([?&;][^=&;#\s"'\\]+=)[^&;#\s"'\\]*`)

// Quote is s as a log line carries it: its credentials blanked out
// (redact.Text) and the value of every query pair it quotes too, a search's
// words among them; cut to its first Max bytes, at a character, with "…"
// after them where there were more; and quoted as a Go string, so that a
// line break or any other control character in it is written as an escape,
// inside the line.
func Quote(s string) string {
	s = queryValue.ReplaceAllString(redact.Text(s), "${1}"+redact.Redacted)
	if len(s) > Max {
		cut := Max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return strconv.Quote(s)
}

// Route is the route pattern r was routed by; where it has none, its path,
// through Quote.
func Route(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
		return rc.RoutePattern()
	}
	return Quote(r.URL.Path)
}
