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
// The Series detail page renders each season as an accordion / list. 404
// when katalog-api has no such series, or the viewer's rating cap leaves it
// out; the episodes the cap leaves out are not listed.
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
		if eps == nil { // katalog-api's 404: no such series, or one the viewer's cap leaves out
			http.Error(w, "not found", http.StatusNotFound)
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
// nothing matches, fall back to the first episode, S01E01.
//
// Specials (season 0) are neither the first nor the next episode unless
// the viewer is inside season 0 already: katalog lists them first, so a
// series nobody had started used to begin with a special. 404 when katalog
// has no such series, or the viewer's rating cap leaves it out; an episode
// the cap leaves out is never the next.
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
		if eps == nil { // katalog-api's 404: no such series, or one the viewer's cap leaves out
			http.Error(w, "not found", http.StatusNotFound)
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
			// No anchor: the first episode.
			writeJSON(w, http.StatusOK, map[string]any{"next": eps[firstEpisode(eps)]})
			return
		}
		next := episodeAfter(eps, anchorIdx)
		if next < 0 {
			writeJSON(w, http.StatusOK, map[string]any{"next": nil, "reason": "end_of_series"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"next":   eps[next],
			"anchor": eps[anchorIdx].ID,
		})
	}
}

// isSpecial reports whether e is a special: an episode of season 0. One
// without a season number is not.
func isSpecial(e katalog.Item) bool {
	return e.SeasonNumber != nil && *e.SeasonNumber == 0
}

// firstEpisode is the index in eps (non-empty, in (season, episode) order)
// where a viewer who has not started the series begins: its first regular
// episode — specials are extras, not the start. A series of nothing but
// specials begins with the first of them.
func firstEpisode(eps []katalog.Item) int {
	for i, e := range eps {
		if !isSpecial(e) {
			return i
		}
	}
	return 0
}

// episodeAfter is the index of the episode that follows eps[anchor]: the
// next one in order, skipping specials unless the viewer is inside season
// 0 (the anchor is a special). -1 at the end of the series.
func episodeAfter(eps []katalog.Item, anchor int) int {
	inSpecials := isSpecial(eps[anchor])
	for i := anchor + 1; i < len(eps); i++ {
		if inSpecials || !isSpecial(eps[i]) {
			return i
		}
	}
	return -1
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
// clamp was added. 404 when katalog-api has no such item, or the viewer's
// rating cap leaves it out.
func itemSegments(kc, streamKC *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		bearer := bearerFrom(r)
		segs, err := kc.ListSegments(r.Context(), bearer, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if segs == nil { // katalog-api's 404: no such item, or one the viewer's cap leaves out
			http.Error(w, "not found", http.StatusNotFound)
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
