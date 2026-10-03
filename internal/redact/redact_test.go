package redact

import (
	"reflect"
	"testing"
)

const jwtSample = "eyJhbGciOiJSUzI1NiIsImtpZCI6ImsxIn0.eyJzdWIiOiJ1c2VyLTEifQ.c2lnbmF0dXJl"

func TestText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"a backdrop with the bearer",
			"/api/v1/items/i1/backdrop?token=" + jwtSample,
			"/api/v1/items/i1/backdrop?token=REDACTED"},
		{"a segment with the stream token among other params",
			"/api/v1/items/i1/play/a0/seg-00012.m4s?stream=dXNlci0xfDE3OTE.c2ln&q=high&caps=avc%2Caac",
			"/api/v1/items/i1/play/a0/seg-00012.m4s?stream=REDACTED&q=high&caps=avc%2Caac"},
		{"the telemetry beacon",
			"/api/v1/play/events?token=" + jwtSample,
			"/api/v1/play/events?token=REDACTED"},
		{"an absolute URL quoted in an error",
			`Get "http://chino-stream:8080/api/play/i1/master.m3u8?stream=abc.def&q=high": dial tcp: no such host`,
			`Get "http://chino-stream:8080/api/play/i1/master.m3u8?stream=REDACTED&q=high": dial tcp: no such host`},
		{"OAuth names", "/cb?code=c1&state=s&access_token=a&id_token=i&refresh_token=r",
			"/cb?code=REDACTED&state=s&access_token=REDACTED&id_token=REDACTED&refresh_token=REDACTED"},
		{"any case, repeated", "/x?Token=a&STREAM=b&stream=c", "/x?Token=REDACTED&STREAM=REDACTED&stream=REDACTED"},
		{"a bare query", "stream=a&q=1", "stream=REDACTED&q=1"},
		{"a matrix parameter", "/x;token=a", "/x;token=REDACTED"},
		{"percent-encoded inside another parameter", "/x?next=%2Fplay%3Fstream%3Dabc", "/x?next=%2Fplay%3Fstream%3DREDACTED"},
		{"an Authorization header quoted", "upstream said 401 to Authorization: Bearer abc.def-ghi_jkl~mno+p/q=",
			"upstream said 401 to Authorization: Bearer REDACTED"},
		{"a bare JWT", "token rejected: " + jwtSample + " (expired)", "token rejected: REDACTED (expired)"},
		{"a JWT in a path", "/debug/" + jwtSample + "/x", "/debug/REDACTED/x"},
		// Left alone.
		{"look-alike names", "/x?upstream=a&streams=b&stream_id=c&tokens=d&tokenize=e",
			"/x?upstream=a&streams=b&stream_id=c&tokens=d&tokenize=e"},
		{"a search for the word", "/api/v1/items?q=token%20ring&type=movie", "/api/v1/items?q=token%20ring&type=movie"},
		{"an empty value", "/x?token=&q=1", "/x?token=&q=1"},
		{"no query", "/api/v1/items/i1/poster", "/api/v1/items/i1/poster"},
		{"the word bearer in prose", "a bearer of bad news; send a Bearer token", "a bearer of bad news; send a Bearer token"},
	}
	for _, tc := range cases {
		if got := Text(tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

func TestValueRedactsEveryString(t *testing.T) {
	in := map[string]any{
		"kind":   "hls_fatal",
		"p_url":  "/api/v1/items/i1/play/audio/0/init.mp4?stream=abc&q=high",
		"p_code": 500.0,
		"p_frag": nil,
		"p_list": []any{"/x?token=" + jwtSample, 1.0, map[string]any{"u": "Bearer zzzzzzzzzzzzzzzzzzzz"}},
		"p_ctx":  map[string]string{"route": "/play?stream=s"},
	}
	want := map[string]any{
		"kind":   "hls_fatal",
		"p_url":  "/api/v1/items/i1/play/audio/0/init.mp4?stream=REDACTED&q=high",
		"p_code": 500.0,
		"p_frag": nil,
		"p_list": []any{"/x?token=REDACTED", 1.0, map[string]any{"u": "Bearer REDACTED"}},
		"p_ctx":  map[string]string{"route": "/play?stream=REDACTED"},
	}
	if got := Value(in); !reflect.DeepEqual(got, want) {
		t.Errorf("Value:\n got %#v\nwant %#v", got, want)
	}
	if in["p_url"] != "/api/v1/items/i1/play/audio/0/init.mp4?stream=abc&q=high" {
		t.Error("Value changed its input")
	}
}
