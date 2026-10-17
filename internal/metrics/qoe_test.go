package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	desktopUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"
	iphoneUA  = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Mobile/15E148 Safari/604.1"
	tizenUA   = "Mozilla/5.0 (SMART-TV; LINUX; Tizen 6.0) AppleWebKit/537.36 (KHTML, like Gecko) 76.0.3809.146/6.0 TV Safari/537.36"
)

// resetQoE empties the playback-quality metrics, so each test sees only its
// own events.
func resetQoE() {
	StartupSeconds.Reset()
	StartupTimeouts.Reset()
	Rebuffers.Reset()
	RebufferSeconds.Reset()
	BandwidthMbps.Reset()
}

// series is what one series holds: a counter's value, a histogram's count
// and sum.
type series struct {
	value float64
	count uint64
	sum   float64
}

// read is the series of c with exactly labels; the zero series when there
// is none.
func read(t *testing.T, c prometheus.Collector, labels map[string]string) series {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			if len(m.GetLabel()) != len(labels) {
				continue
			}
			match := true
			for _, l := range m.GetLabel() {
				if want, ok := labels[l.GetName()]; !ok || want != l.GetValue() {
					match = false
				}
			}
			if match {
				return series{m.GetCounter().GetValue(), m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()}
			}
		}
	}
	return series{}
}

// seriesIn is how many series c has.
func seriesIn(t *testing.T, c prometheus.Collector) int {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range families {
		n += len(f.GetMetric())
	}
	return n
}

func wantHistogram(t *testing.T, c prometheus.Collector, labels map[string]string, count uint64, sum float64) {
	t.Helper()
	if got := read(t, c, labels); got.count != count || got.sum != sum {
		t.Errorf("%v: count %d sum %g, want count %d sum %g", labels, got.count, got.sum, count, sum)
	}
}

func wantCounter(t *testing.T, c prometheus.Collector, labels map[string]string, want float64) {
	t.Helper()
	if got := read(t, c, labels).value; got != want {
		t.Errorf("%v: %g, want %g", labels, got, want)
	}
}

func TestFirstFrameObservesTheStartupByModeAndResume(t *testing.T) {
	resetQoE()
	// The web player: JSON numbers and booleans.
	OnPlayerEvent("first_frame", map[string]any{"startup_ms": 1500.0, "request_ms": 900.0, "mode": "packaged", "resume": false}, desktopUA)
	OnPlayerEvent("first_frame", map[string]any{"startup_ms": 2750.0, "mode": "packaged", "resume": true}, desktopUA)
	// The TV app: every value a string, no mode, no resume.
	OnPlayerEvent("first_frame", map[string]any{"startup_ms": "2300", "client": "chino-androidtv"}, "okhttp/4.12.0")

	wantHistogram(t, StartupSeconds, map[string]string{"mode": "packaged", "resume": "false"}, 1, 1.5)
	wantHistogram(t, StartupSeconds, map[string]string{"mode": "packaged", "resume": "true"}, 1, 2.75)
	wantHistogram(t, StartupSeconds, map[string]string{"mode": "unknown", "resume": "unknown"}, 1, 2.3)
}

func TestFirstFrameTheViewerWaitedOnIsNotObserved(t *testing.T) {
	resetQoE()
	OnPlayerEvent("first_frame", map[string]any{"startup_ms": 40_000.0, "mode": "packaged", "resume": false, "hidden": true}, desktopUA)
	OnPlayerEvent("first_frame", map[string]any{"startup_ms": 9_000.0, "mode": "packaged", "resume": false, "click_to_play": true}, desktopUA)
	OnPlayerEvent("first_frame", map[string]any{"startup_ms": "7000", "mode": "packaged", "hidden": "true"}, desktopUA)
	if n := seriesIn(t, StartupSeconds); n != 0 {
		t.Errorf("%d startup series, want none", n)
	}
}

func TestStartupValuesAreCheckedAndCapped(t *testing.T) {
	resetQoE()
	for _, bad := range []any{-1.0, "-200", "soon", "NaN", "+Inf", nil, true, map[string]any{}} {
		OnPlayerEvent("first_frame", map[string]any{"startup_ms": bad, "mode": "transcode", "resume": false}, desktopUA)
	}
	if n := seriesIn(t, StartupSeconds); n != 0 {
		t.Fatalf("%d startup series from values that are no duration, want none", n)
	}
	OnPlayerEvent("first_frame", map[string]any{"startup_ms": 9e12, "mode": "transcode", "resume": false}, desktopUA)
	OnPlayerEvent("first_frame", map[string]any{"startup_ms": 0.0, "mode": "transcode", "resume": false}, desktopUA)
	wantHistogram(t, StartupSeconds, map[string]string{"mode": "transcode", "resume": "false"}, 2, maxStartupSeconds)
}

func TestLabelsComeFromFixedSets(t *testing.T) {
	resetQoE()
	for _, mode := range []any{"Packaged", "packaged ", "hls", `packaged"}`, 3.0, nil} {
		OnPlayerEvent("first_frame", map[string]any{"startup_ms": 1000.0, "mode": mode, "resume": "maybe"}, desktopUA)
		OnPlayerEvent("startup_timeout", map[string]any{"mode": mode, "phase": "segment"}, desktopUA)
	}
	wantHistogram(t, StartupSeconds, map[string]string{"mode": "unknown", "resume": "unknown"}, 6, 6)
	if n := seriesIn(t, StartupSeconds); n != 1 {
		t.Errorf("%d startup series, want 1 (unknown, unknown)", n)
	}
	wantCounter(t, StartupTimeouts, map[string]string{"mode": "unknown", "phase": "unknown"}, 6)
}

func TestStartupTimeoutCountsWhatWasPending(t *testing.T) {
	resetQoE()
	OnPlayerEvent("startup_timeout", map[string]any{"attempt": 0.0, "phase": "first_segment", "mode": "packaged", "seg_loaded": 4.2e6, "seg_total": 3.5e7}, desktopUA)
	OnPlayerEvent("startup_timeout", map[string]any{"attempt": 1.0, "phase": "first_segment", "mode": "packaged"}, desktopUA)
	OnPlayerEvent("startup_timeout", map[string]any{"phase": "request"}, desktopUA)
	// The iOS app's: an attempt, as a string, and nothing of what was pending.
	OnPlayerEvent("startup_timeout", map[string]any{"attempt": "0", "client": "chino-mobile-ios"}, "")

	wantCounter(t, StartupTimeouts, map[string]string{"mode": "packaged", "phase": "first_segment"}, 2)
	wantCounter(t, StartupTimeouts, map[string]string{"mode": "unknown", "phase": "request"}, 1)
	wantCounter(t, StartupTimeouts, map[string]string{"mode": "unknown", "phase": "unknown"}, 1)
}

func TestRebufferCountsEveryStallAndObservesItsDuration(t *testing.T) {
	resetQoE()
	OnPlayerEvent("rebuffer", map[string]any{"duration_ms": 2500.0, "mode": "packaged", "end": "playing"}, desktopUA)
	OnPlayerEvent("rebuffer", map[string]any{"duration_ms": "500", "mode": "packaged"}, desktopUA)
	// Counted, but no duration to observe.
	OnPlayerEvent("rebuffer", map[string]any{"duration_ms": -40.0, "mode": "packaged"}, desktopUA)
	OnPlayerEvent("rebuffer", map[string]any{"mode": "packaged"}, desktopUA)
	// An hour and more standing still is counted at the cap.
	OnPlayerEvent("rebuffer", map[string]any{"duration_ms": 86_400_000.0, "mode": "remux"}, desktopUA)

	wantCounter(t, Rebuffers, map[string]string{"mode": "packaged"}, 4)
	wantHistogram(t, RebufferSeconds, map[string]string{"mode": "packaged"}, 2, 3)
	wantHistogram(t, RebufferSeconds, map[string]string{"mode": "remux"}, 1, maxRebufferSeconds)
}

func TestBandwidthIsObservedByDeviceClass(t *testing.T) {
	resetQoE()
	OnPlayerEvent("bandwidth", map[string]any{"bps": 2.4e7, "frag_bytes": 4.1e6}, desktopUA)
	OnPlayerEvent("bandwidth", map[string]any{"bps": 6e6}, iphoneUA)
	OnPlayerEvent("bandwidth", map[string]any{"bps": "12000000", "client": "chino-androidtv"}, "okhttp/4.12.0")
	OnPlayerEvent("bandwidth", map[string]any{"bps": 3e6}, tizenUA)
	// No estimate, or none that is one: not observed.
	for _, bad := range []any{0.0, -5e6, "fast", nil} {
		OnPlayerEvent("bandwidth", map[string]any{"bps": bad}, desktopUA)
	}
	// A cached segment can make an estimate absurd: counted at the cap.
	OnPlayerEvent("bandwidth", map[string]any{"bps": 4e13}, desktopUA)

	wantHistogram(t, BandwidthMbps, map[string]string{"device": "desktop"}, 2, 24+maxBandwidthMbps)
	wantHistogram(t, BandwidthMbps, map[string]string{"device": "mobile"}, 1, 6)
	wantHistogram(t, BandwidthMbps, map[string]string{"device": "tv"}, 2, 15)
}

func TestDeviceClass(t *testing.T) {
	for _, c := range []struct{ client, ua, want string }{
		{"chino-androidtv", "okhttp/4.12.0", "tv"},
		{"chino-mobile-android", "Ktor client", "mobile"},
		{"chino-mobile-ios", "", "mobile"},
		{"", desktopUA, "desktop"},
		{"", "Mozilla/5.0 (X11; Linux x86_64; rv:143.0) Gecko/20100101 Firefox/143.0", "desktop"},
		{"", iphoneUA, "mobile"},
		{"", "Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36", "mobile"},
		{"", "Mozilla/5.0 (Linux; Android 14; SM-X710) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36", "mobile"},
		{"", tizenUA, "tv"},
		{"", "Mozilla/5.0 (Web0S; Linux/SmartTV) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/94.0.4606.128 Safari/537.36 WebAppManager", "tv"},
		{"", "Mozilla/5.0 (Linux; Android 12; BRAVIA 4K VH2 Build/STT2.230109.001) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36", "tv"},
		{"", "Mozilla/5.0 (Linux; Android 9; AFTMM Build/PS7633; wv) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36", "tv"},
		{"", "Mozilla/5.0 (X11; Linux armv7l) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36 CrKey/1.56.500000", "tv"},
		{"", "", "desktop"},
	} {
		if got := deviceClass(c.client, c.ua); got != c.want {
			t.Errorf("deviceClass(%q, %q) = %s, want %s", c.client, c.ua, got, c.want)
		}
	}
}

// The playback-quality kinds are counted as every kind is, a kind the
// server does not know is logged and counted as before, and the counters
// there were keep counting as they did.
func TestEveryKindIsStillCountedAndTheOldCountersUnchanged(t *testing.T) {
	resetQoE()
	kind := func(k string) float64 { return read(t, PlayerEvents, map[string]string{"kind": k}).value }
	switches := func() float64 {
		return read(t, QualitySwitches, map[string]string{"from": "high", "to": "medium"}).value
	}
	stalls := func() float64 { return read(t, StallRecoveries, map[string]string{}).value }
	ff, lvl, zap, sw, st := kind("first_frame"), kind("level_switch"), kind("zap_impression"), switches(), stalls()

	OnPlayerEvent("first_frame", map[string]any{"startup_ms": 800.0, "mode": "packaged", "resume": false}, desktopUA)
	OnPlayerEvent("level_switch", map[string]any{"from_height": 2160.0, "to_height": 1080.0, "manual": false}, desktopUA)
	OnPlayerEvent("zap_impression", nil, desktopUA)
	OnPlayerEvent("quality_switch", map[string]any{"from": "high", "to": "medium", "manual": false}, desktopUA)
	OnPlayerEvent("stall_recover", map[string]any{"action": "wait"}, desktopUA)

	for _, c := range []struct {
		what        string
		before, now float64
	}{
		{"first_frame", ff, kind("first_frame")},
		{"level_switch", lvl, kind("level_switch")},
		{"an unknown kind", zap, kind("zap_impression")},
		{"quality_switch", sw, switches()},
		{"stall_recover", st, stalls()},
	} {
		if c.now-c.before != 1 {
			t.Errorf("%s counted %g times, want once", c.what, c.now-c.before)
		}
	}
	// Nothing but first_frame reached the playback-quality metrics.
	if n := seriesIn(t, StartupTimeouts) + seriesIn(t, Rebuffers) + seriesIn(t, RebufferSeconds) + seriesIn(t, BandwidthMbps); n != 0 {
		t.Errorf("%d series from kinds that feed none", n)
	}
	wantHistogram(t, StartupSeconds, map[string]string{"mode": "packaged", "resume": "false"}, 1, 0.8)
}
