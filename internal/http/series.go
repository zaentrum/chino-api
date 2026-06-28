package http

import (
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/katalog"
	"github.com/zaentrum/chino-api/internal/store"
)

// seriesEpisodes returns every episode of a series, grouped by season.
// The Series detail page renders each season as an accordion / list.
func seriesEpisodes(kc *katalog.Client, st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		bearer := bearerFrom(r)
		userID, _ := auth.SubjectFromContext(r.Context())
		eps, err := kc.ListSeriesEpisodes(r.Context(), bearer, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		// Stamp the current user's watched_at on each episode so the
		// EpisodesList can render a checkmark next to ones they've
		// finished. stampWatchedSlice no-ops if userID / st is empty.
		stampWatchedSlice(r.Context(), st, userID, eps)
		// Group by season.
		seasonMap := map[int][]katalog.Item{}
		for _, e := range eps {
			s := 0
			if e.SeasonNumber != nil {
				s = *e.SeasonNumber
			}
			seasonMap[s] = append(seasonMap[s], e)
		}
		var seasons []map[string]any
		nums := make([]int, 0, len(seasonMap))
		for k := range seasonMap {
			nums = append(nums, k)
		}
		sort.Ints(nums)
		for _, n := range nums {
			seasons = append(seasons, map[string]any{
				"season":   n,
				"episodes": seasonMap[n],
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"series_id": id,
			"seasons":   seasons,
			"count":     len(eps),
		})
	}
}

// nextEpisode picks the next episode after the one the user is currently
// watching. If `?after={episodeId}` is provided, take the episode after
// that one (per (season,episode) ordering). Otherwise, use the user's
// most recent playback_progress row for any episode of this series; if
// nothing matches, fall back to S01E01.
func nextEpisode(kc *katalog.Client, st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		seriesID := chi.URLParam(r, "id")
		bearer := bearerFrom(r)
		userID, _ := auth.SubjectFromContext(r.Context())

		eps, err := kc.ListSeriesEpisodes(r.Context(), bearer, seriesID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if len(eps) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"next": nil})
			return
		}

		// Find anchor episode index.
		after := r.URL.Query().Get("after")
		anchorIdx := -1
		if after != "" {
			for i, e := range eps {
				if e.ID == after {
					anchorIdx = i
					break
				}
			}
		}
		if anchorIdx < 0 && st != nil && userID != "" {
			// Take the highest-progress episode of this series the user
			// has touched.
			latest, _ := st.LastWatchedEpisode(r.Context(), userID, episodeIDs(eps))
			if latest != "" {
				for i, e := range eps {
					if e.ID == latest {
						anchorIdx = i
						break
					}
				}
			}
		}

		if anchorIdx < 0 {
			// No anchor: return the first episode.
			writeJSON(w, http.StatusOK, map[string]any{"next": eps[0]})
			return
		}
		if anchorIdx+1 >= len(eps) {
			writeJSON(w, http.StatusOK, map[string]any{"next": nil, "reason": "end_of_series"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"next":   eps[anchorIdx+1],
			"anchor": eps[anchorIdx].ID,
		})
	}
}

func episodeIDs(eps []katalog.Item) []string {
	out := make([]string, len(eps))
	for i, e := range eps {
		out[i] = e.ID
	}
	return out
}

// itemSegments surfaces the analyzer-detected segments for an item so the
// player can render Skip-Intro / Skip-Credits buttons and scrub-bar ticks.
//
// Before responding we clamp each segment's end_ms to the packaged
// content's real duration (from katalog-stream's /play/info, which reads
// the manifest's ffprobe-derived durationMs). TIDB authors segments
// against the TMDB-rounded runtime, which is typically 30-90s longer
// than the actual file; without this pass, credits/intro overlays
// extend past the seek bar and the auto-play-next watcher would never
// fire because the playhead can't reach the stale end_ms. Segments
// that start past the real end are dropped entirely.
//
// If /play/info fails or returns 0 (e.g. item not yet packaged), we
// fall through with the raw segments — same behaviour as before this
// clamp was added.
func itemSegments(kc, streamKC *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		bearer := bearerFrom(r)
		segs, err := kc.ListSegments(r.Context(), bearer, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if durMs := streamKC.PlayInfoDurationMs(r.Context(), bearer, id); durMs > 0 {
			clamped := make([]katalog.Segment, 0, len(segs))
			for _, s := range segs {
				if s.StartMs >= durMs {
					continue
				}
				if s.EndMs > durMs {
					s.EndMs = durMs
				}
				clamped = append(clamped, s)
			}
			segs = clamped
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"item_id":  id,
			"segments": segs,
			"count":    len(segs),
		})
	}
}
