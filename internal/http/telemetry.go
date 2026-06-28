package http

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/metrics"
)

const maxTelemetryBody = 256 * 1024 // 256 KB per batch.

type telemetryEvent struct {
	TS      int64          `json:"ts"`
	Kind    string         `json:"kind"`
	ItemID  string         `json:"itemId,omitempty"`
	Payload map[string]any `json:"payload,omitempty"`
}

type telemetryBatch struct {
	SessionID string           `json:"sessionId"`
	Events    []telemetryEvent `json:"events"`
}

// postTelemetry accepts a batched event upload from the player and emits
// one structured JSON log line per event so the cluster's log aggregator
// can index them. We deliberately don't persist to a database — playback
// events are observability, not state.
func postTelemetry(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxTelemetryBody)
	defer r.Body.Close()

	var b telemetryBatch
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		if errors.Is(err, io.EOF) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	sub, _ := auth.SubjectFromContext(r.Context())
	serverTS := time.Now().UnixMilli()

	for _, e := range b.Events {
		// Bump Prometheus counters first so the dashboard updates even
		// when the log aggregator is behind.
		metrics.OnPlayerEvent(e.Kind, e.Payload)

		line := map[string]any{
			"src":       "player",
			"user":      sub,
			"sessionId": b.SessionID,
			"server_ts": serverTS,
			"client_ts": e.TS,
			"kind":      e.Kind,
			"itemId":    e.ItemID,
		}
		for k, v := range e.Payload {
			line["p_"+k] = v
		}
		buf, _ := json.Marshal(line)
		log.Println(string(buf))
	}
	w.WriteHeader(http.StatusNoContent)
}
