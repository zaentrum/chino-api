package http

import (
	"net/http"
	"testing"

	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
)

// streamRouter is chino-api with chino-stream at streamURL and OIDC on (an
// issuer never asked: the requests carry a stream token or nothing).
func streamRouter(t *testing.T, streamURL string) http.Handler {
	t.Helper()
	h, err := NewRouter(config.Config{
		OIDCIssuer:       "http://127.0.0.1:1/realms/none",
		OIDCAudience:     "chino-web",
		OIDCEnabled:      true,
		KatalogBaseURL:   "http://katalog-api.invalid",
		ArtworkBaseURL:   "http://katalog-manager.invalid",
		StreamBaseURL:    streamURL,
		StreamSigningKey: signingKey,
	}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// A package's WebVTT subtitle renditions (sN) are proxied to chino-stream
// like its video and audio renditions: in the stream-token group, the
// query (the stream token, caps, q) riding on to chino-stream, which
// checks the token again.
func TestSubtitleRenditionsAreProxiedWithTheStreamToken(t *testing.T) {
	const playlist = "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.000,\nseg-00001.vtt\n#EXT-X-ENDLIST\n"
	stream := newFake(t, map[string]string{
		"/api/play/i1/s0/playlist.m3u8":  playlist,
		"/api/play/i1/s12/seg-00001.vtt": "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHello.\n",
		"/api/play/i1/v0/seg-00001.m4s":  "moof",
	})
	h := streamRouter(t, stream.URL)
	tok := streamToken(t)

	for path, upstream := range map[string]string{
		"/api/v1/items/i1/play/s0/playlist.m3u8":  "/api/play/i1/s0/playlist.m3u8",
		"/api/v1/items/i1/play/s12/seg-00001.vtt": "/api/play/i1/s12/seg-00001.vtt",
		"/api/v1/items/i1/play/v0/seg-00001.m4s":  "/api/play/i1/v0/seg-00001.m4s",
	} {
		before := len(stream.requests())
		w := do(h, "GET", path+"?stream="+tok+"&caps=avc,hvc,aac", nil)
		if w.Code != http.StatusOK || w.Body.Len() == 0 {
			t.Errorf("%s: %d %q", path, w.Code, w.Body)
			continue
		}
		reqs := stream.requests()
		if len(reqs) != before+1 {
			t.Fatalf("%s: %d upstream requests", path, len(reqs)-before)
		}
		r := reqs[len(reqs)-1]
		if r.URL.Path != upstream || r.URL.Query().Get("stream") != tok || r.URL.Query().Get("caps") != "avc,hvc,aac" {
			t.Errorf("%s reached chino-stream as %s", path, r.URL)
		}
	}
	if w := do(h, "GET", "/api/v1/items/i1/play/s0/playlist.m3u8?stream="+tok, nil); w.Body.String() != playlist {
		t.Errorf("playlist body %q", w.Body)
	}

	// No credential, or a forged one: 401, and chino-stream is not asked.
	before := len(stream.requests())
	for _, q := range []string{"", "?stream=" + tok[:len(tok)-2] + "xx"} {
		for _, path := range []string{"/api/v1/items/i1/play/s0/playlist.m3u8", "/api/v1/items/i1/play/s12/seg-00001.vtt"} {
			if w := do(h, "GET", path+q, nil); w.Code != http.StatusUnauthorized {
				t.Errorf("%s%s: %d, want 401", path, q, w.Code)
			}
		}
	}
	if n := len(stream.requests()) - before; n != 0 {
		t.Errorf("%d upstream requests without a valid credential", n)
	}

	// Only sN with digits and .vtt segments are subtitle renditions.
	for _, path := range []string{"/api/v1/items/i1/play/s0/seg-00001.m4s", "/api/v1/items/i1/play/sx/seg-00001.vtt",
		"/api/v1/items/i1/play/s0/seg-x.vtt"} {
		if w := do(h, "GET", path+"?stream="+tok, nil); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, w.Code)
		}
	}
}

// A playback session is pinned to a package version: chino-stream writes
// v=<versionId> onto the URIs of its masters and playlists, and serves each
// request of the session from that version. chino-api passes the query on
// as it came, so v= reaches chino-stream unchanged beside the stream token
// and caps: on a title's master, its packaged renditions and its on-the-fly
// ladder, and on an extra's master and the extra's on-the-fly ladder.
func TestTheVersionPinRidesOnToChinoStream(t *testing.T) {
	const version = "f1f1f1f1-0000-4000-8000-0000000000f1"
	extra := "/api/play/i1/extras/" + testExtra
	routes := map[string]string{
		"/api/v1/items/i1/play/master.m3u8":                          "/api/play/i1/master.m3u8",
		"/api/v1/items/i1/play/v0/playlist.m3u8":                     "/api/play/i1/v0/playlist.m3u8",
		"/api/v1/items/i1/play/v0/seg-00001.m4s":                     "/api/play/i1/v0/seg-00001.m4s",
		"/api/v1/items/i1/play/high/index.m3u8":                      "/api/play/i1/high/index.m3u8",
		"/api/v1/items/i1/play/high/3.m4s":                           "/api/play/i1/high/3.m4s",
		"/api/v1/items/i1/extras/" + testExtra + "/play/master.m3u8": extra + "/master.m3u8",
		"/api/v1/items/i1/extras/" + testExtra + "/play/high/3.m4s":  extra + "/high/3.m4s",
	}
	upstream := map[string]string{}
	for _, p := range routes {
		upstream[p] = "bytes of " + p
	}
	stream := newFake(t, upstream)
	h := streamRouter(t, stream.URL)
	query := "stream=" + streamToken(t) + "&caps=avc:1080,hvc:2160,aac&v=" + version

	for path, want := range routes {
		before := len(stream.requests())
		w := do(h, "GET", path+"?"+query, nil)
		reqs := stream.requests()
		if w.Code != http.StatusOK || w.Body.String() != upstream[want] || len(reqs) != before+1 {
			t.Errorf("%s: %d %q, %d upstream requests", path, w.Code, w.Body, len(reqs)-before)
			continue
		}
		if r := reqs[len(reqs)-1]; r.URL.Path != want || r.URL.RawQuery != query {
			t.Errorf("%s reached chino-stream as %s, want %s?%s", path, r.URL, want, query)
		}
	}
}
