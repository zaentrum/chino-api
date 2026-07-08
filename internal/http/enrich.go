package http

import (
	"context"
	"strings"
	"time"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/katalog"
	"github.com/zaentrum/chino-api/internal/store"
)

// artworkTokenTTL bounds a poster/backdrop stream token. Longer than the player
// token — artwork is heavily cached and non-sensitive, and each list fetch
// re-mints one anyway.
const artworkTokenTTL = 24 * time.Hour

// streamSigner mints the ?stream= tokens stamped onto artwork URLs. Set once at
// router construction (SetStreamSigner); nil disables stamping.
var streamSigner *auth.Signer

// SetStreamSigner wires the HMAC signer used to authenticate <img> artwork
// requests, which can't carry an Authorization header.
func SetStreamSigner(s *auth.Signer) { streamSigner = s }

// stampArtworkTokens appends a per-user ?stream=<token> to each item's poster
// and backdrop URL so a plain <img src> authenticates via StreamMiddleware (the
// token then rides through the artwork proxy to katalog-manager-api, which
// shares the signing key). One token per call, bound to userID. No-op without a
// signer or userID.
func stampArtworkTokens(userID string, items []*katalog.Item) {
	if streamSigner == nil || userID == "" || len(items) == 0 {
		return
	}
	token, _ := streamSigner.Mint(userID, artworkTokenTTL)
	if token == "" {
		return
	}
	for _, it := range items {
		if it == nil {
			continue
		}
		it.PosterURL = appendStreamToken(it.PosterURL, token)
		it.BackdropURL = appendStreamToken(it.BackdropURL, token)
	}
}

func appendStreamToken(u, token string) string {
	if u == "" || strings.Contains(u, "stream=") {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "stream=" + token
}

// stampWatched annotates each item with the current user's watched_at
// timestamp (when present in watched_history). Mutates in place; safe
// on nil items / empty slice. Best-effort: a DB error just leaves
// WatchedAt unset rather than failing the whole list response.
func stampWatched(ctx context.Context, st *store.Store, userID string, items []*katalog.Item) {
	if userID == "" || len(items) == 0 {
		return
	}
	// Authenticate the <img> artwork requests (poster/backdrop) — needs only
	// the signer + userID, so it runs even if the watched-history store is nil.
	stampArtworkTokens(userID, items)
	if st == nil {
		return
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if it != nil && it.ID != "" {
			ids = append(ids, it.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	watched, err := st.WatchedAtBatch(ctx, userID, ids)
	if err != nil || len(watched) == 0 {
		return
	}
	for _, it := range items {
		if it == nil {
			continue
		}
		if ts, ok := watched[it.ID]; ok {
			t := ts
			it.WatchedAt = &t
		}
	}
}

// stampWatchedSlice is a convenience over stampWatched for a
// concrete-typed slice (listItems gets back a []katalog.Item).
func stampWatchedSlice(ctx context.Context, st *store.Store, userID string, items []katalog.Item) {
	if len(items) == 0 {
		return
	}
	ptrs := make([]*katalog.Item, len(items))
	for i := range items {
		ptrs[i] = &items[i]
	}
	stampWatched(ctx, st, userID, ptrs)
}

// listUnwatched fetches up to `limit` items the user has NOT finished,
// starting at `offset`, backfilling from later katalog pages so a Home
// rail stays full after the user marks some of its items watched (#189).
//
// katalog doesn't know per-user watched state, so we over-fetch a page,
// stamp watched_at, drop the watched ones, and keep paging until we have
// `limit` survivors or katalog runs dry. Bounded to maxPages so a user
// who has watched almost everything can't turn one rail into an unbounded
// scan — the rail just comes back short, which is correct.
func listUnwatched(
	ctx context.Context,
	st *store.Store,
	userID string,
	fetchPage func(lim, off int) ([]katalog.Item, error),
	limit, offset int,
) ([]katalog.Item, error) {
	const maxPages = 4
	if limit <= 0 {
		limit = 20
	}
	// Over-fetch per page: with a typical low watched ratio, 2× usually
	// satisfies the rail in a single round-trip.
	batch := limit * 2
	out := make([]katalog.Item, 0, limit)
	off := offset
	for page := 0; page < maxPages && len(out) < limit; page++ {
		got, err := fetchPage(batch, off)
		if err != nil {
			// Surface the error only when we have nothing — otherwise
			// return what we gathered so the rail still renders.
			if len(out) == 0 {
				return nil, err
			}
			break
		}
		if len(got) == 0 {
			break // katalog exhausted
		}
		stampWatchedSlice(ctx, st, userID, got)
		for i := range got {
			if got[i].WatchedAt == nil {
				out = append(out, got[i])
				if len(out) >= limit {
					break
				}
			}
		}
		if len(got) < batch {
			break // last katalog page
		}
		off += len(got)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
