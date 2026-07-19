// Package metrics is chino-api's Prometheus surface. Counters incremented
// from the telemetry handler so the player's event stream is queryable in
// Grafana.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// Coarse-grained counter for every player event we receive. The `kind`
	// label is bounded by the events the client emits today
	// (mount/play/pause/waiting/media_error/stall_recover/resume_accept/
	// resume_decline/quality_switch); cardinality stays low.
	PlayerEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chino_player_events_total",
		Help: "Player events received by chino-api, grouped by event kind.",
	}, []string{"kind"})

	// Media-decode errors broken down by code name (ABORTED / NETWORK /
	// DECODE / SRC_NOT_SUPPORTED). Useful to spot codec/transcode
	// regressions.
	MediaErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chino_player_media_errors_total",
		Help: "MediaError events surfaced by the browser player.",
	}, []string{"code"})

	// `quality_switch` events: tracks how often the player drops a rung
	// (manual + auto-downgrade). `from`/`to` are high/medium/low.
	QualitySwitches = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chino_player_quality_switches_total",
		Help: "Quality-rung changes initiated by the player.",
	}, []string{"from", "to"})

	// Resume offer outcomes — useful for product analytics ("how often do
	// people pick up where they left off?").
	ResumeOutcomes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chino_player_resume_outcomes_total",
		Help: "User response to the resume-watching offer.",
	}, []string{"outcome"}) // outcome = accept | decline

	// Hard-stall recoveries (the 8 s-no-progress watcher firing).
	StallRecoveries = promauto.NewCounter(prometheus.CounterOpts{
		Name: "chino_player_stall_recoveries_total",
		Help: "Times the player forced a stream reconnect after a wedged transcode.",
	})

	// Progress save calls.
	ProgressSaves = promauto.NewCounter(prometheus.CounterOpts{
		Name: "chino_player_progress_saves_total",
		Help: "Successful playback-progress upserts.",
	})

	// HTTP traffic — gives us a global request rate + latency dashboard
	// without depending on the event stream.
	HTTPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chino_api_http_requests_total",
		Help: "HTTP requests served by chino-api, by route + status.",
	}, []string{"route", "status"})

	HTTPDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "chino_api_http_request_duration_seconds",
		Help:    "End-to-end HTTP request latency.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"route", "status"})
)

// Middleware records request rate + latency. Captures the chi-route
// pattern (not the literal URL) so cardinality stays bounded.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = r.URL.Path
		}
		status := strconv.Itoa(sw.status)
		HTTPRequests.WithLabelValues(route, status).Inc()
		HTTPDuration.WithLabelValues(route, status).Observe(time.Since(start).Seconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush forwards http.Flusher through the wrapper — embedding the
// ResponseWriter INTERFACE promotes only its three methods, so without this
// the SSE bridge's `w.(http.Flusher)` assertion fails ("streaming
// unsupported") for every wrapped handler.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.NewResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Handler returns the Prometheus scrape handler. Mounted at /metrics
// without auth — chino-api's pod is unreachable from outside the cluster
// and the scrape job is in the cluster Prometheus.
func Handler() http.Handler {
	return promhttp.Handler()
}

// OnPlayerEvent bumps the right counters for one telemetry event. Called
// from the telemetry POST handler.
func OnPlayerEvent(kind string, payload map[string]any) {
	PlayerEvents.WithLabelValues(kind).Inc()
	switch kind {
	case "media_error":
		code, _ := payload["name"].(string)
		if code == "" {
			code = "unknown"
		}
		MediaErrors.WithLabelValues(code).Inc()
	case "quality_switch":
		from, _ := payload["from"].(string)
		to, _ := payload["to"].(string)
		if from == "" {
			from = "unknown"
		}
		if to == "" {
			to = "unknown"
		}
		QualitySwitches.WithLabelValues(from, to).Inc()
	case "resume_accept":
		ResumeOutcomes.WithLabelValues("accept").Inc()
	case "resume_decline":
		ResumeOutcomes.WithLabelValues("decline").Inc()
	case "stall_recover":
		StallRecoveries.Inc()
	}
}
