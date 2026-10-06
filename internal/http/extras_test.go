package http

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
)

// testExtra is an extra's id, a UUID as katalog-manager gives them.
const testExtra = "1b5c2a8e-6f0d-4c3e-9a51-2d7f0c4b8e01"

// extraRoutes are the routes of one extra under its title's
// /api/v1/items/{id}/extras/{extraId}/play/, one of each shape: its
// package's, and those of the on-the-fly ladder chino-stream's master of it
// names for a client that decodes none of the package's rungs.
var extraRoutes = []string{"master.m3u8", "v0/playlist.m3u8", "a0/playlist.m3u8", "s0/playlist.m3u8",
	"v1/iframes.m3u8", "v0/init.mp4", "a0/init.mp4", "v0/seg-00001.m4s", "a0/seg-00004.m4s", "s0/seg-00002.vtt",
	"high/index.m3u8", "medium/init.mp4", "low/3.m4s", "audio/0/index.m3u8", "audio/1/init.mp4", "audio/1/2.m4s"}

// An extra's master and the renditions it names, packaged or on the fly, are
// proxied to chino-stream under the title's id, in the stream-token group,
// as the title's own routes of their shape are: /api/v1/items/{id}/extras/
// {extraId}/play/<route> is chino-stream's /api/play/{id}/extras/{extraId}/
// <route> as /api/v1/items/{id}/play/<route> is its /api/play/{id}/<route>,
// the query (the stream token, caps, q) riding on, and chino-stream's answer
// coming back as it is — its 404 for an extra that is not the title's too.
func TestExtrasAreProxiedWithTheStreamToken(t *testing.T) {
	base := "/api/play/i1/extras/" + testExtra + "/"
	upstream := map[string]string{}
	for _, route := range extraRoutes {
		upstream[base+route] = "bytes of " + route
		upstream["/api/play/i1/"+route] = "bytes of the title's " + route
	}
	stream := newFake(t, upstream)
	h := streamRouter(t, stream.URL)
	tok := streamToken(t)

	for _, route := range extraRoutes {
		for path, want := range map[string]string{
			"/api/v1/items/i1/extras/" + testExtra + "/play/" + route: base + route,
			"/api/v1/items/i1/play/" + route:                          "/api/play/i1/" + route,
		} {
			before := len(stream.requests())
			w := do(h, "GET", path+"?stream="+tok+"&caps=avc,aac&q=v1", nil)
			if w.Code != http.StatusOK || w.Body.String() != upstream[want] {
				t.Errorf("%s: %d %q", path, w.Code, w.Body)
				continue
			}
			reqs := stream.requests()
			if len(reqs) != before+1 {
				t.Fatalf("%s: %d upstream requests", path, len(reqs)-before)
			}
			r := reqs[len(reqs)-1]
			if q := r.URL.Query(); r.URL.Path != want || q.Get("stream") != tok || q.Get("caps") != "avc,aac" || q.Get("q") != "v1" {
				t.Errorf("%s reached chino-stream as %s", path, r.URL)
			}
		}
	}

	// chino-stream's 404 — the extra is another title's, or not packaged —
	// comes back as it is.
	if w := do(h, "GET", "/api/v1/items/i2/extras/"+testExtra+"/play/master.m3u8?stream="+tok, nil); w.Code != http.StatusNotFound {
		t.Errorf("another title's extra: %d %q, want chino-stream's 404", w.Code, w.Body)
	}

	// No credential, or a forged one: 401 on every route of an extra, as on
	// the title's route of its shape, and chino-stream is not asked.
	before := len(stream.requests())
	for _, q := range []string{"", "?stream=" + tok[:len(tok)-2] + "xx"} {
		for _, route := range extraRoutes {
			for _, path := range []string{"/api/v1/items/i1/extras/" + testExtra + "/play/" + route, "/api/v1/items/i1/play/" + route} {
				if w := do(h, "GET", path+q, nil); w.Code != http.StatusUnauthorized {
					t.Errorf("%s%s: %d, want 401", path, q, w.Code)
				}
			}
		}
	}
	// An extra id that is no UUID, on every route of an extra; a rendition or
	// a rung of no route's shape (no stream copy either: a package is never
	// stream-copied); and the routes of a title an extra has none of: 404,
	// and chino-stream is not asked either.
	var unrouted []string
	for _, id := range []string{"not-an-extra", testExtra[:35], testExtra + "0", "..%2F..%2Fi2"} {
		for _, route := range extraRoutes {
			unrouted = append(unrouted, "/api/v1/items/i1/extras/"+id+"/play/"+route)
		}
	}
	for _, route := range []string{"s0/init.mp4", "v0/seg-00001.vtt", "x0/playlist.m3u8", "v0/index.m3u8",
		"highx/index.m3u8", "xlow/init.mp4", "high/playlist.m3u8", "high/seg-00001.m4s", "copy/index.m3u8",
		"audio/x/index.m3u8", "audio/0/seg-00001.m4s", "trickplay/thumbnails.vtt", "info"} {
		unrouted = append(unrouted, "/api/v1/items/i1/extras/"+testExtra+"/play/"+route)
	}
	for _, path := range append(unrouted, "/api/v1/items/i1/extras/"+testExtra+"/progress") {
		if w := do(h, "GET", path+"?stream="+tok, nil); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d %q, want 404", path, w.Code, w.Body)
		}
	}
	if n := len(stream.requests()) - before; n != 0 {
		t.Errorf("%d upstream requests for what is no extra's route or without a valid credential", n)
	}
}

// A capped viewer gets the same 404 on the routes of a title's extras as on
// the title's own when its cap does not allow the title (an adult one, an
// unrated one, an id of none): with its bearer and with its stream token,
// before anything goes upstream but the question of what the cap allows. An
// extra of a title the cap allows plays, and so does every extra for a
// viewer without a cap.
func TestACappedViewerGets404OnTheExtrasOfATitleAboveItsCap(t *testing.T) {
	is := newIssuer(t)
	h, _, stream, _ := ratedRouter(t, is)
	signer, err := auth.NewSigner(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	streamTok, _ := signer.MintCapped("kid-1", 12, time.Hour)
	capped := bearerWith(t, is, "kid-1", 12)
	adult := bearerWith(t, is, "adult-1", nil)
	creds := []struct {
		name, query string
		header      http.Header
	}{{"a bearer capped at 12", "", capped}, {"a stream token capped at 12", "?stream=" + streamTok, nil}}

	for _, route := range extraRoutes {
		for _, title := range []string{"adult", "unrated", "no-such-title"} {
			path := "/api/v1/items/" + title + "/extras/" + testExtra + "/play/" + route
			for _, c := range creds {
				own := do(h, "GET", "/api/v1/items/"+title+"/play/master.m3u8"+c.query, c.header)
				stream.take()
				w := do(h, "GET", path+c.query, c.header)
				if w.Code != http.StatusNotFound || w.Body.String() != "not found\n" || w.Code != own.Code || w.Body.String() != own.Body.String() {
					t.Errorf("%s, %s: %d %q; the title's own master %d %q", path, c.name, w.Code, w.Body, own.Code, own.Body)
				}
				if got := stream.take(); len(got) != 0 {
					t.Errorf("%s, %s: went to chino-stream %q", path, c.name, got)
				}
			}
		}
		for _, tc := range []struct {
			title, viewer string
			header        http.Header
		}{{"kid", "capped", capped}, {"adult", "uncapped", adult}} {
			path := "/api/v1/items/" + tc.title + "/extras/" + testExtra + "/play/" + route
			stream.take()
			w := do(h, "GET", path, tc.header)
			got := stream.take()
			if w.Code != http.StatusOK || w.Body.String() != "media bytes" || len(got) != 1 ||
				!strings.HasPrefix(got[0], "GET /api/play/"+tc.title+"/extras/"+testExtra+"/"+route) {
				t.Errorf("%s for the %s viewer: %d %q, chino-stream got %q", path, tc.viewer, w.Code, w.Body, got)
			}
		}
	}
}

// A title's detail lists its extras beside its trailers, as the clients read
// it: each extra as katalog-api sends it, with local: true and the play_path
// of its master here; the trailers the links to online videos they were, url
// and all. An extra's play_path plays: it is the extra's master on
// chino-stream. A title without extras has no extras field.
func TestItemDetailListsTheExtrasBesideTheTrailers(t *testing.T) {
	const trailers = `[{"site":"YouTube","external_id":"x1","url":"https://www.youtube.com/watch?v=x1","title":"Official Trailer"}]`
	kat := newFake(t, map[string]string{
		"/api/v1/items/m1": `{"id":"m1","type":"movie","title":"A Film","trailers":` + trailers + `,
			"extras":[{"id":"` + testExtra + `","kind":"trailer","title":"Trailer","language":"en","duration_ms":33000}]}`,
		"/api/v1/items/m2": `{"id":"m2","type":"movie","title":"Another Film","trailers":` + trailers + `}`,
	})
	stream := newFake(t, map[string]string{"/api/play/m1/extras/" + testExtra + "/master.m3u8": "#EXTM3U\n"})
	h, err := NewRouter(config.Config{
		OIDCIssuer: "http://127.0.0.1:1/realms/none", OIDCAudience: "chino-web", OIDCEnabled: false,
		KatalogBaseURL: kat.URL, ArtworkBaseURL: "http://katalog-manager.invalid", StreamBaseURL: stream.URL,
		StreamSigningKey: signingKey,
	}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}

	w := do(h, "GET", "/api/v1/items/m1", nil)
	var detail map[string]json.RawMessage
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &detail) != nil {
		t.Fatalf("detail: %d %s", w.Code, w.Body)
	}
	playPath := "/api/v1/items/m1/extras/" + testExtra + "/play/master.m3u8"
	for field, want := range map[string]string{
		"trailers": trailers,
		"extras": `[{"id":"` + testExtra + `","kind":"trailer","title":"Trailer","language":"en","duration_ms":33000,
			"local":true,"play_path":"` + playPath + `"}]`,
	} {
		var got, wantJSON any
		if err := json.Unmarshal(detail[field], &got); err != nil {
			t.Fatalf("%s: %v in %s", field, err, w.Body)
		}
		if err := json.Unmarshal([]byte(want), &wantJSON); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, wantJSON) {
			t.Errorf("%s:\n got %s\nwant %s", field, detail[field], want)
		}
	}
	if w := do(h, "GET", playPath+"?caps=avc,aac", nil); w.Code != http.StatusOK || w.Body.String() != "#EXTM3U\n" {
		t.Errorf("the play_path: %d %q", w.Code, w.Body)
	}

	w = do(h, "GET", "/api/v1/items/m2", nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"extras"`) || !strings.Contains(w.Body.String(), `"trailers"`) {
		t.Errorf("a title without extras: %d %s", w.Code, w.Body)
	}
}
