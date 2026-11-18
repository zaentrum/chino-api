// Package logline is what chino-api's own log lines about a request carry
// of it: the route it was routed by, and each value that came from a caller
// or an upstream (a path, a reason, a URL, the first bytes of an answer)
// through Quote, so that every such line stays one line of a bounded length.
package logline

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/redact"
)

// Max is the most bytes of a value a line carries.
const Max = 256

// What Quote blanks out besides redact.Text's, each to Redacted:
var (
	// queryValue matches the value of a query's pair in the text, of a URL,
	// a request URI or a bare query, and of a URL's fragment (#access_token=).
	queryValue = regexp.MustCompile(`([?&;#][^=&;#\s"'\\]+=)[^&;#\s"'\\]*`)
	// userinfo matches the user and password before a URL's host.
	userinfo = regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://)[^/?#\s"'@]+@`)
	// scheme matches the credential after an Authorization scheme, Basic or
	// Bearer, of any length.
	scheme = regexp.MustCompile(`(?i)(\b(?:basic|bearer)\s+)[A-Za-z0-9._~+/=-]+`)
	// jsonField matches the value of a JSON field named for a credential, in
	// any case: token, access_token, accessToken, refresh_token,
	// refreshToken, id_token, idToken, stream_token, jwt, session, password,
	// secret, client_secret, clientSecret, api_key, apikey, and the names of
	// the headers of header.
	jsonField = regexp.MustCompile(`(?i)("(?:token|access_token|accessToken|refresh_token|refreshToken|id_token|idToken|stream_token|jwt|session|` +
		`password|secret|client_secret|clientSecret|api_key|apikey|` +
		`authorization|proxy-authorization|cookie|set-cookie|x-account-deletion-token|x-api-key)"\s*:\s*)(?:"(?:[^"\\]|\\.)*"|[^,}\]\s]+)`)
)

// A header that carries a credential, quoted in text: header matches its
// name and the colon or the equals sign after it, as "Name: ", Go's
// "Name:[", "name=", "name = ", or with a quote between ("Name": ,
// 'Name': ); headerValue says how much of what follows is its value, which
// Quote blanks.
var header = regexp.MustCompile(`(?i)\b(proxy-authorization|authorization|set-cookie|cookie|x-account-deletion-token|x-api-key)["']?[ \t]*[:=][ \t]*`)

// The shapes of such a header's value, each at the start of what follows the
// name (headerValue):
var (
	// quoted is a value in quotes, whole.
	quoted = regexp.MustCompile(`^(?:"(?:[^"\\\r\n]|\\.)*"|'[^'\r\n]*')`)
	// listed is a value in Go's form, [v1 v2], to its ].
	listed = regexp.MustCompile(`^\[[^\]\r\n]*`)
	// authParams is an Authorization's scheme and its parameters, name=value
	// (Digest's), each value a token or in quotes.
	authParams = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]+[ \t]+` + param + `(?:[ \t]*,[ \t]*` + param + `)*`)
	// credential is an Authorization's scheme and its token, or one unbroken
	// token, where the value ends there: a word of prose after the name
	// ("authorization: the bearer has …") is no credential.
	credential = regexp.MustCompile(`^((?:[A-Za-z0-9!#$%&'*+.^_|~-]+[ \t]+)?` + unbroken + `)` + valueEnd)
	// cookiePairs is a Cookie's or a Set-Cookie's name=value pairs, each
	// value to its ; (one with a space, or in braces, among them), a part of
	// it in quotes whole.
	cookiePairs = regexp.MustCompile(`^` + pair + `(?:[ \t]*;[ \t]*` + pair + `)*`)
	// oneToken is one unbroken token, where the value ends there.
	oneToken = regexp.MustCompile(`^(` + unbroken + `)` + valueEnd)
	// toEnd is a value to its end: the line's, or a quote's or a bracket's.
	toEnd = regexp.MustCompile(`^[^\r\n"\]}]+`)
)

const (
	param    = `[A-Za-z0-9!#$%&'*+.^_|~-]+=(?:"(?:[^"\\\r\n]|\\.)*"|[^\s,"'\]})]*)`
	pair     = `[A-Za-z0-9!#$%&'*+.^_|~-]+=(?:"(?:[^"\\\r\n]|\\.)*"|[^;\r\n])*`
	unbroken = `[^\s"'\[\]{}(),;]+`
	valueEnd = `(?:$|[\r\n"'\]}),;])`
)

// headerValue is how many bytes of rest, what follows the name of a header
// that carries a credential, are its value: one in quotes or in Go's form,
// whole; of an Authorization or a Proxy-Authorization named as HTTP writes
// it all of it, to its end, and named in any other case one in its shape, a
// scheme and its parameters, or a scheme and its token or one token where
// the value ends there; of a Cookie or a Set-Cookie its name=value pairs, or
// one token where the value ends there; of any other header all of it, to
// its end. 0 where what follows has none of the shapes: prose that names
// the header in lower case ("missing authorization: the bearer has …"),
// which stays.
func headerValue(name, rest string) int {
	for _, shape := range []*regexp.Regexp{quoted, listed} {
		if loc := shape.FindStringIndex(rest); loc != nil {
			return loc[1]
		}
	}
	var shapes []*regexp.Regexp
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization":
		if name == "Authorization" || name == "Proxy-Authorization" {
			shapes = []*regexp.Regexp{toEnd}
			break
		}
		shapes = []*regexp.Regexp{authParams, credential}
	case "cookie", "set-cookie":
		shapes = []*regexp.Regexp{cookiePairs, oneToken}
	default:
		shapes = []*regexp.Regexp{toEnd}
	}
	for _, shape := range shapes {
		if m := shape.FindStringSubmatchIndex(rest); m != nil {
			if len(m) > 2 {
				return m[3] // the value, without the end after it
			}
			return m[1]
		}
	}
	return 0
}

// blankHeaders is s with the value of each header that carries a credential
// it quotes blanked out, as headerValue has it.
func blankHeaders(s string) string {
	var b strings.Builder
	for {
		m := header.FindStringSubmatchIndex(s)
		if m == nil {
			return b.String() + s
		}
		b.WriteString(s[:m[1]])
		value := headerValue(s[m[2]:m[3]], s[m[1]:])
		if value > 0 {
			b.WriteString(redact.Redacted)
		}
		s = s[m[1]+value:]
	}
}

// Quote is s as a log line carries it, its credentials blanked out: what
// redact.Text blanks, a URL's userinfo, the credential of a Basic or a Bearer
// of any length, a credential header quoted in text in its value's shape, a
// JSON field named for a credential, and the value of every query pair (a
// search's words among them) and of a fragment's; cut to its first Max
// bytes, at a character, with "…" after them where there were more; and
// quoted as a Go string, so that a line break or any other control character
// in it is written as an escape, inside the line.
func Quote(s string) string {
	s = blankHeaders(s)
	s = jsonField.ReplaceAllString(s, `${1}"`+redact.Redacted+`"`)
	s = userinfo.ReplaceAllString(s, "${1}"+redact.Redacted+"@")
	s = scheme.ReplaceAllString(s, "${1}"+redact.Redacted)
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
