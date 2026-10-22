package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/katalog"
	"github.com/zaentrum/chino-api/internal/metrics"
	"github.com/zaentrum/chino-api/internal/store"
)

type progressBody struct {
	PositionSec int `json:"position_sec"`
	DurationSec int `json:"duration_sec"`
	// Dismiss: the title is taken off Continue Watching - saved at its end,
	// so the feed drops it (an episode's next takes its place), but not
	// marked watched: the viewer did not finish it.
	Dismiss bool `json:"dismiss,omitempty"`
	// PlayedMsAgo: how long before this request the position was played, in
	// milliseconds - a position a phone kept while it had no connection,
	// sent as its age so that the phone's clock does not matter. It is the
	// position at now() minus the age, written only over an older one: a
	// newer position another device wrote meanwhile stays. Without it the
	// position is now's, written as before.
	PlayedMsAgo *int64 `json:"played_ms_ago,omitempty"`
}

// getProgress returns { position_sec } for the current user + item: the
// resume point. 200 with zero is the normal "never watched" state, and the
// one of a title the viewer finished (store.Finished: its last 5 % or 2
// minutes), which Play starts over - that one with finished: true, so a
// client can tell it from one never begun. The item's own row:
// an episode of a file that holds several has the file's position because
// each write of it went to each of them (postProgress).
func getProgress(s *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		itemID := chi.URLParam(r, "id")
		if itemID == "" {
			http.Error(w, "missing item id", http.StatusBadRequest)
			return
		}
		res, err := s.GetResume(r.Context(), userID, itemID)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		body := map[string]any{"position_sec": res.PositionSec}
		if res.Finished {
			body["finished"] = true
		}
		writeJSON(w, http.StatusOK, body)
	}
}

// postProgress upserts playback progress. Called every ~10s by the
// player; idempotent under high call rate. The position is the file's: it
// is written for every episode of the file the item plays (fileOf), the
// same for each, and read back per episode. A position in the title's
// finished end (store.Finished) marks it watched too, as the player's own
// POST …/watched does: one rule, here, whichever client played it - but
// for a dismiss, which only takes the title off Continue Watching. A
// position sent with its age (played_ms_ago) is written only where the row
// is older, and marks watched only what it wrote: a film finished on the TV
// stays finished when the phone's earlier second comes in later.
func postProgress(s *store.Store, kc *katalog.Client) http.HandlerFunc {
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
		ids := fileOf(r, s, kc, userID, itemID)
		finished := !body.Dismiss && store.Finished(body.PositionSec, body.DurationSec)
		if body.PlayedMsAgo == nil {
			if err := s.SaveProgress(r.Context(), userID, ids, body.PositionSec, body.DurationSec); err != nil {
				http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if finished {
				if err := s.MarkWatched(r.Context(), userID, ids...); err != nil {
					http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
					return
				}
			}
		} else {
			// An age below zero (a clock set back since) is now's.
			ago := time.Duration(max(*body.PlayedMsAgo, 0)) * time.Millisecond
			won, err := s.SaveProgressPlayed(r.Context(), userID, ids, body.PositionSec, body.DurationSec, ago)
			if err != nil {
				http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if finished && len(won) > 0 {
				if err := s.MarkWatchedPlayed(r.Context(), userID, ago, won...); err != nil {
					http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}
		metrics.ProgressSaves.Inc()
		w.WriteHeader(http.StatusNoContent)
	}
}
