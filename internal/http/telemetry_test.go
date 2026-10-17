package http

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/zaentrum/chino-api/internal/metrics"
)

// A batch of the playback-quality events, as the web player sends it from a
// phone, through the telemetry handler to what GET /metrics serves: each
// event logged as sent, each measure observed under its labels, a value
// that is none not observed.
func TestTelemetryFeedsThePlaybackQualityMetrics(t *testing.T) {
	for _, reset := range []interface{ Reset() }{metrics.StartupSeconds, metrics.StartupTimeouts, metrics.Rebuffers, metrics.RebufferSeconds, metrics.BandwidthMbps} {
		reset.Reset()
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	body := `{"sessionId":"s1","events":[` +
		`{"ts":1,"kind":"first_frame","itemId":"i1","payload":{"startup_ms":1800,"request_ms":1200,"mode":"packaged","height":1080,"bitrate":6000000,"resume":true,"start_bucket":"5-30m","hidden":false,"click_to_play":false}},` +
		`{"ts":2,"kind":"bandwidth","itemId":"i1","payload":{"bps":24000000,"frag_bytes":4200000,"frag_load_ms":1400,"trigger":"first_frame"}},` +
		`{"ts":3,"kind":"rebuffer","itemId":"i1","payload":{"duration_ms":3200,"mode":"packaged","cause":"waiting","end":"playing"}},` +
		`{"ts":4,"kind":"rebuffer","itemId":"i1","payload":{"duration_ms":-7,"mode":"packaged"}},` +
		`{"ts":5,"kind":"startup_timeout","itemId":"i2","payload":{"attempt":0,"phase":"first_segment","mode":"packaged","seg_loaded":4194304,"seg_total":35000000}},` +
		`{"ts":6,"kind":"level_switch","itemId":"i1","payload":{"from_height":1080,"to_height":720,"manual":false}}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/play/events", strings.NewReader(body))
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Mobile/15E148 Safari/604.1")
	w := httptest.NewRecorder()
	postTelemetry(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("telemetry: %d %s", w.Code, w.Body)
	}

	scrape := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	exposition := scrape.Body.String()
	for _, line := range []string{
		`chino_player_startup_seconds_bucket{mode="packaged",resume="true",le="2"} 1`,
		`chino_player_startup_seconds_bucket{mode="packaged",resume="true",le="1"} 0`,
		`chino_player_startup_seconds_sum{mode="packaged",resume="true"} 1.8`,
		`chino_player_bandwidth_mbps_sum{device="mobile"} 24`,
		`chino_player_bandwidth_mbps_bucket{device="mobile",le="25"} 1`,
		`chino_player_rebuffers_total{mode="packaged"} 2`,
		`chino_player_rebuffer_seconds_count{mode="packaged"} 1`,
		`chino_player_rebuffer_seconds_sum{mode="packaged"} 3.2`,
		`chino_player_startup_timeouts_total{mode="packaged",phase="first_segment"} 1`,
	} {
		if !strings.Contains(exposition, line+"\n") {
			t.Errorf("GET /metrics lacks %s", line)
		}
	}
	// Labels name no user, item or address.
	for _, leak := range []string{"i1", "i2", "s1", "iPhone"} {
		for _, l := range strings.Split(exposition, "\n") {
			if strings.HasPrefix(l, "chino_player_") && strings.Contains(l, leak) {
				t.Errorf("a series carries %q: %s", leak, l)
			}
		}
	}
	// The log keeps what the player sent, the negative duration included.
	for _, want := range []string{`"kind":"first_frame"`, `"p_request_ms":1200`, `"p_seg_total":35000000`, `"p_duration_ms":-7`, `"p_to_height":720`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("telemetry log lacks %s:\n%s", want, logs.String())
		}
	}
}
