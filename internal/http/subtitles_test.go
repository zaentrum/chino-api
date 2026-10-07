package http

import (
	"encoding/json"
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
