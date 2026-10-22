package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/katalog"
)

// maxAvailableIDs is how many titles one POST /me/available may ask about:
// what katalog-api's /visible takes at a time.
const maxAvailableIDs = 500

// availableLookups is how many of an uncapped viewer's titles are looked up
// in the catalog at once.
const availableLookups = 8

// postAvailable answers, in one request, which of the titles a client keeps
// the caller may watch now. The phone's downloads ask it as the app starts
// and comes to the front, and before one of them plays, and delete a title
// the answer leaves out: the library decides, for every account. The body is
// {"ids": [...]}, at most maxAvailableIDs; the answer {"ids": [...]}, the
// ids asked about that the caller may watch, in their order, each once. A
// capped viewer is held to its cap as on every route of one title
// (gate.visibleIDs: a title above it, an unrated one the catalog's setting
// hides and an id of no title are left out); for a viewer without a cap the
// catalog's item route decides, a title it holds. When the catalog cannot
// say, the answer is 502 with no ids - and a client then deletes nothing.
func postAvailable(g gate, kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body struct {
			IDs []string `json:"ids"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
			if errors.Is(err, io.EOF) {
				http.Error(w, "empty body", http.StatusBadRequest)
				return
			}
			http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		ids := distinctIDs(body.IDs)
		if len(ids) > maxAvailableIDs {
			http.Error(w, "at most 500 ids at a time", http.StatusBadRequest)
			return
		}
		var allowed []string
		var err error
		if _, capped := auth.MaxRatingFromContext(r.Context()); capped {
			allowed, err = g.visibleIDs(r.Context(), ids)
		} else {
			allowed, err = held(r.Context(), kc, bearerFrom(r), ids)
		}
		if err != nil {
			catalogUnavailable(w, err)
			return
		}
		if allowed == nil {
			allowed = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ids": allowed})
	}
}

// distinctIDs is ids trimmed, without the empty ones and without repeats, in
// their order.
func distinctIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// held is which of ids name a title the catalog holds, for a viewer without
// a cap, as the catalog's item route answers them - availableLookups at a
// time, in their order. One the catalog does not answer for fails the whole
// answer: a title is never left out for want of an answer.
func held(ctx context.Context, kc *katalog.Client, bearer string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	there := make([]bool, len(ids))
	next := make(chan int)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	for range min(availableLookups, len(ids)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				it, err := kc.GetItem(ctx, bearer, ids[i])
				if err != nil {
					mu.Lock()
					if first == nil {
						first = err
						cancel()
					}
					mu.Unlock()
					continue
				}
				there[i] = it != nil
			}
		}()
	}
send:
	for i := range ids {
		select {
		case next <- i:
		case <-ctx.Done():
			break send
		}
	}
	close(next)
	wg.Wait()
	if first != nil {
		return nil, first
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	for i, id := range ids {
		if there[i] {
			out = append(out, id)
		}
	}
	return out, nil
}
