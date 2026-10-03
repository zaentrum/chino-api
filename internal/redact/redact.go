// Package redact keeps credentials out of what chino-api writes down: its
// request log, the telemetry lines it logs, the bug reports it files and
// the upstream errors it passes on.
//
// Clients put credentials in URLs where they cannot set a header: the OIDC
// access token as ?token= (an <img src>, a sendBeacon) and the signed stream
// token as ?stream= (every media URL; it authorises a user's media for
// hours). Those URLs reached the request log verbatim — about 170 lines a
// day with a live bearer on the demo, thousands with a stream token — and
// players report the URLs that failed in their telemetry and bug reports.
package redact

import "regexp"

// Redacted is what a credential is replaced with.
const Redacted = "REDACTED"

var (
	// credentialParam matches a credential-carrying query parameter and
	// its value, in a URL, a request URI, a bare query or text quoting
	// one: the names the stream group accepts plus the usual OAuth ones,
	// case-insensitive, also percent-encoded inside another parameter
	// (…%3Fstream%3D…).
	credentialParam = regexp.MustCompile(`(?i)((?:^|[?&;]|%3f|%26)` +
		`(?:token|stream|access_token|id_token|refresh_token|code|client_secret|password|api_key|apikey)` +
		`(?:=|%3d))[^&#\s"'\\]+`)
	// bearer matches an Authorization header value quoted in text. Real
	// tokens are long; the minimum length leaves prose ("a bearer of …",
	// "Bearer token") alone.
	bearer = regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/=-]{16,}`)
	// jwt matches a bare JWT (header.payload.signature) anywhere else.
	jwt = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)
)

// Text returns s with every credential it recognises replaced by Redacted:
// the value of a credential query parameter, a "Bearer <token>", a bare
// JWT. Everything else is left as it is.
func Text(s string) string {
	s = credentialParam.ReplaceAllString(s, "${1}"+Redacted)
	s = bearer.ReplaceAllString(s, "${1}"+Redacted)
	return jwt.ReplaceAllString(s, Redacted)
}

// Value returns v with Text applied to every string in it, through maps and
// slices — for decoded JSON. Other values come back as they are.
func Value(v any) any {
	switch x := v.(type) {
	case string:
		return Text(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = Value(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = Value(e)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(x))
		for k, e := range x {
			out[k] = Text(e)
		}
		return out
	}
	return v
}
