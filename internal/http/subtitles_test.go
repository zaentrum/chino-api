package http

import (
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
