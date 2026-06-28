package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/metrics"
	"github.com/zaentrum/chino-api/internal/store"
)

type progressBody struct {
	PositionSec int `json:"position_sec"`
	DurationSec int `json:"duration_sec"`
}

// getProgress returns { position_sec } for the current user + item.
// 200 with zero is the normal "never watched" state.
func getProgress(s *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		itemID := chi.URLParam(r, "id")
		if itemID == "" {
			http.Error(w, "missing item id", http.StatusBadRequest)
			return
		}
		pos, err := s.GetProgress(r.Context(), userID, itemID)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"position_sec": pos})
	}
}

// postProgress upserts playback progress. Called every ~10s by the
// player; idempotent under high call rate.
func postProgress(s *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		itemID := chi.URLParam(r, "id")
		if itemID == "" {
			http.Error(w, "missing item id", http.StatusBadRequest)
			return
		}
		defer r.Body.Close()
		var body progressBody
		dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
		if err := dec.Decode(&body); err != nil {
			if errors.Is(err, io.EOF) {
				http.Error(w, "empty body", http.StatusBadRequest)
				return
			}
			http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.SaveProgress(r.Context(), userID, itemID, body.PositionSec, body.DurationSec); err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		metrics.ProgressSaves.Inc()
		w.WriteHeader(http.StatusNoContent)
	}
}
