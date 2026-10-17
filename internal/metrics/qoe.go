package metrics

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Playback quality as the players measure it: how long a title takes to
// show its first frame, the starts given up on, the stalls mid-play and the
// bandwidth the player sees - the baseline ADR-0013's live rungs are judged
// against. Every label comes from a small fixed set below, never a user, an
// item or an address; what does not fit one is "unknown".
var (
	// From first_frame: the player opening (or Try again) to its first
	// frame. A start the page was hidden during, or one the viewer had to
	// click to start, is logged but not observed - that wait is not the
	// platform's.
	StartupSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "chino_player_startup_seconds",
		Help:    "Time from the player opening to its first frame, by play mode and whether it resumed.",
		Buckets: []float64{0.25, 0.5, 1, 2, 3, 5, 8, 12, 20, 30},
	}, []string{"mode", "resume"})

	// From startup_timeout: the player gave up before a first frame, by
	// what it was still waiting for.
	StartupTimeouts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chino_player_startup_timeouts_total",
		Help: "Starts the player gave up on before a first frame, by play mode and the phase pending.",
	}, []string{"mode", "phase"})

	// From rebuffer: a stall that began mid-play, not at the start. The
	// counter counts each; the histogram those with a duration.
	Rebuffers = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chino_player_rebuffers_total",
		Help: "Stalls that began mid-play, by play mode.",
	}, []string{"mode"})

	RebufferSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "chino_player_rebuffer_seconds",
		Help:    "How long playback stood still in a stall that began mid-play, by play mode.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 4, 8, 15, 30, 60},
	}, []string{"mode"})

	// From bandwidth: the player's own estimate while it plays.
	BandwidthMbps = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "chino_player_bandwidth_mbps",
		Help:    "The player's bandwidth estimate while playing, in Mbit/s, by device class.",
		Buckets: []float64{1, 2, 4, 8, 15, 25, 40, 80},
	}, []string{"device"})
)

// What a value may be at most; above it, it is counted at the cap. A
// negative one, or one that is no number, is not observed.
const (
	maxStartupSeconds  = 600    // 10 min: the player gives up after 20 s of trying
	maxRebufferSeconds = 3600   // an hour standing still
	maxBandwidthMbps   = 10_000 // 10 Gbit/s
)

var (
	playModes = map[string]bool{"packaged": true, "passthrough": true, "remux": true, "transcode": true}
	// What a start that timed out was still waiting for: the stream not
	// asked for yet, its playlists, the init segment, the first media
	// segment, or a frame from what arrived.
	startupPhases = map[string]bool{"request": true, "playlist": true, "init": true, "first_segment": true, "decode": true}
)

func observeStartup(p map[string]any) {
	if hidden, _ := flag(p, "hidden"); hidden {
		return
	}
	if clicked, _ := flag(p, "click_to_play"); clicked {
		return
	}
	ms, ok := number(p, "startup_ms")
	if !ok {
		return
	}
	if s, ok := clamped(ms/1000, maxStartupSeconds); ok {
		StartupSeconds.WithLabelValues(modeLabel(p), resumeLabel(p)).Observe(s)
	}
}

func observeRebuffer(p map[string]any) {
	mode := modeLabel(p)
	Rebuffers.WithLabelValues(mode).Inc()
	ms, ok := number(p, "duration_ms")
	if !ok {
		return
	}
	if s, ok := clamped(ms/1000, maxRebufferSeconds); ok {
		RebufferSeconds.WithLabelValues(mode).Observe(s)
	}
}

func observeBandwidth(p map[string]any, userAgent string) {
	bps, ok := number(p, "bps")
	if !ok || bps <= 0 { // no estimate is not an estimate of zero
		return
	}
	mbps, _ := clamped(bps/1e6, maxBandwidthMbps)
	client, _ := p["client"].(string)
	BandwidthMbps.WithLabelValues(deviceClass(client, userAgent)).Observe(mbps)
}

func modeLabel(p map[string]any) string {
	if m, _ := p["mode"].(string); playModes[m] {
		return m
	}
	return "unknown"
}

func phaseLabel(p map[string]any) string {
	if ph, _ := p["phase"].(string); startupPhases[ph] {
		return ph
	}
	return "unknown"
}

func resumeLabel(p map[string]any) string {
	resumed, ok := flag(p, "resume")
	switch {
	case !ok:
		return "unknown"
	case resumed:
		return "true"
	default:
		return "false"
	}
}

var (
	tvAgent     = regexp.MustCompile(`smart-?tv|tizen|web0s|webos|netcast|hbbtv|bravia|android ?tv|google ?tv|crkey|apple ?tv|roku|(^|[^a-z])aft[a-z]`)
	mobileAgent = regexp.MustCompile(`mobi|android|iphone|ipad|ipod`)
)

// deviceClass sorts a player into desktop, mobile or tv: by the client its
// events name (the TV and mobile apps send client: chino-androidtv,
// chino-mobile-android, chino-mobile-ios), else by the request's
// User-Agent. What is not recognised as a TV, a phone or a tablet is
// desktop - an iPad that asks as a Mac included.
func deviceClass(client, userAgent string) string {
	c := strings.ToLower(client)
	switch {
	case strings.Contains(c, "tv") || strings.Contains(c, "tizen"):
		return "tv"
	case strings.Contains(c, "mobile"):
		return "mobile"
	}
	ua := strings.ToLower(userAgent)
	switch {
	case tvAgent.MatchString(ua): // before mobile: a TV's agent may say Android
		return "tv"
	case mobileAgent.MatchString(ua):
		return "mobile"
	}
	return "desktop"
}

// number reads a numeric payload field: a JSON number, or the decimal
// string the TV and mobile apps send every value as. ok is false for
// anything else, NaN and the infinities included.
func number(p map[string]any, key string) (float64, bool) {
	var f float64
	switch v := p[key].(type) {
	case float64:
		f = v
	case int:
		f = float64(v)
	case int64:
		f = float64(v)
	case string:
		x, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		f = x
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// flag reads a boolean payload field: true or false, or its string.
func flag(p map[string]any, key string) (bool, bool) {
	switch v := p[key].(type) {
	case bool:
		return v, true
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		return b, err == nil
	}
	return false, false
}

// clamped is v capped at max; ok is false for a negative v, which is no
// duration and no rate.
func clamped(v, max float64) (float64, bool) {
	if v < 0 {
		return 0, false
	}
	return math.Min(v, max), true
}
