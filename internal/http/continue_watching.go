package http

import (
	"net/http"
	"sync"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/katalog"
	"github.com/zaentrum/chino-api/internal/store"
)

// continueWatchingItem extends a katalog.Item with the user's saved
// position so the client can render a progress bar without a follow-up
// /progress fetch. For episode items, SeriesTitle carries the parent
// series' title so the chino-web Continue Watching row can show
// "How I Met Your Mother — S01E05 — Okay Awesome" without a second
// client fetch.
//
// UpNext is true when this row was substituted for a *finished* episode
// (we replaced the just-finished item with the series' next episode).
// The web layer renders these without a progress bar — they're a fresh
// continuation, not a half-watched item.
type continueWatchingItem struct {
	katalog.Item
	PositionSec int    `json:"position_sec"`
	DurationSec int    `json:"duration_sec"`
	SeriesTitle string `json:"series_title,omitempty"`
	UpNext      bool   `json:"up_next,omitempty"`
}

// continueWatching returns the most recently watched items for the
// current user. Resolves each progress row's metadata via the katalog
// HTTP client in parallel — capped via a small worker pool so we don't
// fan out unbounded.
//
// Handling of finished rows (position within 60 s of duration):
//   - Movie: dropped (user already finished it; no continuation).
//   - Episode: replaced with the next episode in the parent series. The
//     substituted card has position 0, no progress bar (UpNext=true),
//     and clicking plays from the start. If the just-finished episode
//     was the last of the series, the row is dropped.
func continueWatching(st *store.Store, kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		bearer := bearerFrom(r)

		rows, err := st.ListContinueWatching(r.Context(), userID, 40)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Resolve each row in parallel. For finished episodes we also
		// fetch the series' episode list to substitute the next one;
		// that's an extra katalog round trip but only fires for rows
		// flagged Finished.
		out := make([]continueWatchingItem, len(rows))
		ok := make([]bool, len(rows))
		var wg sync.WaitGroup
		sem := make(chan struct{}, 6)
		for i, row := range rows {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, p store.ProgressRow) {
				defer wg.Done()
				defer func() { <-sem }()
				item, err := kc.GetItem(r.Context(), bearer, p.ItemID)
				if err != nil || item == nil {
					return
				}
				entry, keep := buildCWEntry(r, kc, st, userID, bearer, item, p)
				if !keep {
					return
				}
				out[i] = entry
				ok[i] = true
			}(i, row)
		}
		wg.Wait()

		// Compact in-place, preserving order.
		compact := out[:0]
		seen := map[string]bool{}
		for i, valid := range ok {
			if !valid {
				continue
			}
			// Dedup by item id — a "just-finished ep5 → ep6" substitution
			// could collide with an actual in-progress ep6 row from
			// another series we haven't started, or with an older
			// finished ep4 → ep5 if the user keeps re-watching the
			// pilot. Keep the first (most recent).
			id := out[i].Item.ID
			if seen[id] {
				continue
			}
			seen[id] = true
			compact = append(compact, out[i])
			if len(compact) >= 20 {
				break
			}
		}

		// Stamp WatchedAt on the surfaced items so the MediaCard shows
		// the badge consistently (including the substituted next-episode
		// cards, which may themselves already be watched if the user is
		// jumping around).
		stampWatched(r.Context(), st, userID, cwItems(compact))

		writeJSON(w, http.StatusOK, map[string]any{"items": compact})
	}
}

// buildCWEntry produces a continueWatchingItem for one progress row.
// Returns keep=false when the row should be dropped (finished movie, or
// finished episode with no follow-up).
func buildCWEntry(r *http.Request, kc *katalog.Client, st *store.Store, userID, bearer string, item *katalog.Item, p store.ProgressRow) (continueWatchingItem, bool) {
	if !p.Finished {
		entry := continueWatchingItem{
			Item:        *item,
			PositionSec: p.PositionSec,
			DurationSec: p.DurationSec,
		}
		if item.Type == "episode" && item.ParentID != "" {
			if parent, perr := kc.GetItem(r.Context(), bearer, item.ParentID); perr == nil && parent != nil {
				entry.SeriesTitle = parent.Title
			}
		}
		return entry, true
	}

	// Finished. Movies → drop; only episodes get a continuation card.
	if item.Type != "episode" || item.ParentID == "" {
		return continueWatchingItem{}, false
	}
	eps, err := kc.ListSeriesEpisodes(r.Context(), bearer, item.ParentID)
	if err != nil || len(eps) == 0 {
		return continueWatchingItem{}, false
	}
	nextIdx := -1
	for i, e := range eps {
		if e.ID == item.ID {
			nextIdx = i + 1
			break
		}
	}
	if nextIdx <= 0 || nextIdx >= len(eps) {
		// Just-watched ep wasn't found in the series listing, or it was
		// the last episode. Either way, no continuation card.
		return continueWatchingItem{}, false
	}
	// Walk forward to the first UNWATCHED episode after the finished
	// one. Without this, a user who's already binged the whole series
	// once gets that finished episode's immediate next neighbour back
	// on the Next Up rail — even though they finished THAT too. So we
	// skip ahead until we find something genuinely fresh.
	candidateIDs := make([]string, 0, len(eps)-nextIdx)
	for j := nextIdx; j < len(eps); j++ {
		candidateIDs = append(candidateIDs, eps[j].ID)
	}
	watched, _ := st.WatchedAtBatch(r.Context(), userID, candidateIDs)
	for ; nextIdx < len(eps); nextIdx++ {
		if _, alreadyWatched := watched[eps[nextIdx].ID]; !alreadyWatched {
			break
		}
	}
	if nextIdx >= len(eps) {
		// Series fully watched — no card to surface.
		return continueWatchingItem{}, false
	}
	next := eps[nextIdx]
	entry := continueWatchingItem{
		Item:        next,
		PositionSec: 0,
		DurationSec: 0,
		UpNext:      true,
	}
	if parent, perr := kc.GetItem(r.Context(), bearer, item.ParentID); perr == nil && parent != nil {
		entry.SeriesTitle = parent.Title
	}
	return entry, true
}

// cwItems returns pointers to the Item field of each entry so
// stampWatched can mutate them in place.
func cwItems(items []continueWatchingItem) []*katalog.Item {
	out := make([]*katalog.Item, len(items))
	for i := range items {
		out[i] = &items[i].Item
	}
	return out
}
