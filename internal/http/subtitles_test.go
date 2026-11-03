package http

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zaentrum/chino-api/internal/katalog"
)

// A sidecar is listed at its .vtt URL with the format that URL serves: a
// SubRip file is served as WebVTT (chino-stream), so it is listed as webvtt;
// every other format is as the catalog has it.
func TestASidecarIsListedWithWhatItsURLServes(t *testing.T) {
	for format, want := range map[string]string{
		"srt": "webvtt", "SRT": "webvtt", "webvtt": "webvtt", "": "", "pgs": "pgs", "vobsub": "vobsub",
	} {
		got := sidecarEntry(katalog.Subtitle{ID: "s1", Lang: "eng", Format: format})
		if got.Format != want || got.URL != "/api/v1/play/subs/s1.vtt" || got.ID != "s1" || got.Lang != "eng" {
			t.Errorf("format %q: %+v, want format %q at /api/v1/play/subs/s1.vtt", format, got, want)
		}
	}
}

// A forced subtitle reaches the clients as forced, the others without the
// field, so a player can switch it on by itself for the language it is in.
func TestASidecarSaysWhetherItIsForced(t *testing.T) {
	forced, _ := json.Marshal(sidecarEntry(katalog.Subtitle{ID: "s1", Lang: "eng", Format: "webvtt", Forced: true}))
	full, _ := json.Marshal(sidecarEntry(katalog.Subtitle{ID: "s2", Lang: "eng", Format: "webvtt"}))
	if !strings.Contains(string(forced), `"forced":true`) {
		t.Errorf("forced sidecar: %s", forced)
	}
	if strings.Contains(string(full), `"forced"`) {
		t.Errorf("full sidecar: %s", full)
	}
}

// A styled subtitle is listed as every subtitle is, at its .vtt URL with its
// format, and its styled original is offered beside it in styled: the
// script's .ass URL and format (katalog-api's styled_format, not sent
// twice). A subtitle without one, or of a styled format chino-stream does
// not serve, has no styled field.
func TestAStyledSubtitleIsOfferedBesideItsFile(t *testing.T) {
	got, _ := json.Marshal(sidecarEntry(katalog.Subtitle{ID: "s1", Lang: "eng", Format: "webvtt", StyledFormat: "ass"}))
	if want := `{"id":"s1","lang":"eng","format":"webvtt","url":"/api/v1/play/subs/s1.vtt",` +
		`"styled":{"url":"/api/v1/play/subs/s1.ass","format":"ass"}}`; string(got) != want {
		t.Errorf("styled:\n got %s\nwant %s", got, want)
	}
	for _, format := range []string{"", "usf"} {
		got, _ := json.Marshal(sidecarEntry(katalog.Subtitle{ID: "s2", Lang: "eng", Format: "webvtt", StyledFormat: format}))
		if want := `{"id":"s2","lang":"eng","format":"webvtt","url":"/api/v1/play/subs/s2.vtt"}`; string(got) != want {
			t.Errorf("styled format %q:\n got %s\nwant %s", format, got, want)
		}
	}
}

// A font is listed at its URL, whose extension is its format (bin for a font
// of none chino-stream knows), with its names: an empty list when it has
// none.
func TestAFontIsListedAtItsURL(t *testing.T) {
	for format, want := range map[string]string{"ttf": "f1.ttf", "otf": "f1.otf", "ttc": "f1.ttc", "woff": "f1.woff",
		"woff2": "f1.woff2", "": "f1.bin", "eot": "f1.bin"} {
		got := fontEntry(katalog.Font{ID: "f1", Format: format})
		if got.URL != "/api/v1/play/fonts/"+want || got.Format != format || got.Names == nil || len(got.Names) != 0 {
			t.Errorf("format %q: %+v, want it at /api/v1/play/fonts/%s with no names", format, got, want)
		}
	}
	if got := fontEntry(katalog.Font{ID: "f2", Format: "otf", Names: []string{"Example Serif"}}); len(got.Names) != 1 {
		t.Errorf("a font's names: %+v", got)
	}
}

// The subtitles list of a title whose package keeps styled originals offers
// each beside its subtitle, and the package's fonts in their order, each at
// its URL with its names; a title without fonts, or none of whose subtitles
// is offered a styled original, is listed as before, with no fonts field.
// The item itself says a subtitle's styled format as katalog-api does, and
// sends no fonts.
func TestTheSubtitlesListOffersTheStyledOriginalsAndTheFonts(t *testing.T) {
	kat := newFake(t, map[string]string{
		"/api/v1/items/m1": `{"id":"m1","type":"movie","title":"A Film","subtitles":[
			{"id":"s1","lang":"en","label":"English","format":"webvtt","default":true,"styled_format":"ass"},
			{"id":"s2","lang":"de","format":"webvtt"}],
			"fonts":[{"id":"f1","format":"ttf","names":["Example Sans","ExampleSans-Regular"]},{"id":"f2","names":[]}]}`,
		"/api/v1/items/m2": `{"id":"m2","type":"movie","title":"Plain","subtitles":[{"id":"s3","lang":"en","format":"webvtt"}]}`,
		// fonts of no styled subtitle chino-stream serves
		"/api/v1/items/m3": `{"id":"m3","type":"movie","title":"Plain Too","subtitles":[
			{"id":"s4","lang":"en","format":"webvtt","styled_format":"usf"}],"fonts":[{"id":"f3","format":"ttf","names":["Example Sans"]}]}`,
	})
	h := router(t, kat.URL, "http://katalog-manager.invalid", false)

	w := do(h, "GET", "/api/v1/items/m1/subtitles", nil)
	var got, want any
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("m1's subtitles: %d %s", w.Code, w.Body)
	}
	_ = json.Unmarshal([]byte(`{"subtitles":[
		{"id":"s1","lang":"en","label":"English","format":"webvtt","default":true,"url":"/api/v1/play/subs/s1.vtt",
		 "styled":{"url":"/api/v1/play/subs/s1.ass","format":"ass"}},
		{"id":"s2","lang":"de","format":"webvtt","url":"/api/v1/play/subs/s2.vtt"}],
		"fonts":[{"id":"f1","format":"ttf","names":["Example Sans","ExampleSans-Regular"],"url":"/api/v1/play/fonts/f1.ttf"},
		 {"id":"f2","names":[],"url":"/api/v1/play/fonts/f2.bin"}]}`), &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("m1's subtitles:\n got %s\nwant %v", w.Body, want)
	}
	if w := do(h, "GET", "/api/v1/items/m2/subtitles", nil); w.Code != http.StatusOK ||
		w.Body.String() != `{"subtitles":[{"id":"s3","lang":"en","format":"webvtt","url":"/api/v1/play/subs/s3.vtt"}]}`+"\n" {
		t.Errorf("a title without fonts: %d %s", w.Code, w.Body)
	}
	if w := do(h, "GET", "/api/v1/items/m3/subtitles", nil); w.Code != http.StatusOK ||
		w.Body.String() != `{"subtitles":[{"id":"s4","lang":"en","format":"webvtt","url":"/api/v1/play/subs/s4.vtt"}]}`+"\n" {
		t.Errorf("a title whose fonts are of no styled subtitle offered: %d %s", w.Code, w.Body)
	}
	w = do(h, "GET", "/api/v1/items/m1", nil)
	var detail struct {
		Subtitles []map[string]any `json:"subtitles"`
		Fonts     any              `json:"fonts"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &detail) != nil || len(detail.Subtitles) != 2 ||
		detail.Subtitles[0]["styled_format"] != "ass" || detail.Subtitles[0]["styled"] != nil || detail.Fonts != nil {
		t.Errorf("the item: %d %s", w.Code, w.Body)
	}
}

// A styled original and a font are proxied to chino-stream at their URLs, in
// the stream-token group as a subtitle is, the query riding on; a font's
// ETag revalidates through the proxy.
func TestStyledOriginalsAndFontsAreProxied(t *testing.T) {
	stream := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/play/subs/s1.ass":
			w.Header().Set("Content-Type", "text/x-ssa; charset=utf-8")
			_, _ = io.WriteString(w, "[Script Info]\n")
		case r.URL.Path == "/api/play/fonts/f1.ttf" && r.Header.Get("If-None-Match") == `"e1"`:
			w.WriteHeader(http.StatusNotModified)
		case r.URL.Path == "/api/play/fonts/f1.ttf", r.URL.Path == "/api/play/fonts/f2.bin":
			w.Header().Set("Content-Type", "font/ttf")
			w.Header().Set("ETag", `"e1"`)
			_, _ = io.WriteString(w, "font bytes")
		default:
			http.NotFound(w, r)
		}
	})
	h := streamRouter(t, stream.URL)
	tok := streamToken(t)
	for path, want := range map[string]string{
		"/api/v1/play/subs/s1.ass":  "[Script Info]\n",
		"/api/v1/play/fonts/f1.ttf": "font bytes",
		"/api/v1/play/fonts/f2.bin": "font bytes",
	} {
		w := do(h, "GET", path+"?stream="+tok, nil)
		if w.Code != http.StatusOK || w.Body.String() != want {
			t.Errorf("%s: %d %q, want %q", path, w.Code, w.Body, want)
		}
		if w := do(h, "GET", path, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without a credential: %d", path, w.Code)
		}
	}
	got := stream.take()
	sort.Strings(got)
	if want := []string{"GET /api/play/fonts/f1.ttf?stream=" + tok, "GET /api/play/fonts/f2.bin?stream=" + tok,
		"GET /api/play/subs/s1.ass?stream=" + tok}; !reflect.DeepEqual(got, want) {
		t.Errorf("chino-stream got %q, want %q", got, want)
	}
	if w := do(h, "GET", "/api/v1/play/fonts/f1.ttf?stream="+tok, http.Header{"If-None-Match": {`"e1"`}}); w.Code != http.StatusNotModified {
		t.Errorf("a font revalidated: %d", w.Code)
	}
}
