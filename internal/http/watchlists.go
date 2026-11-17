package http

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/katalog"
	"github.com/zaentrum/chino-api/internal/store"
)

// Multiple named watchlists. These handlers own /me/watchlists* and
// REPLACE the three legacy /me/watchlist* routes (which now operate on
// the caller's default list via EnsureDefaultList, keeping their exact
// {items:[...]} / 204 shapes for already-installed mobile/TV builds).
// /me/likes stays on the old flagSpec path — untouched.

// maxListName / minListName bound a list name after trimming. The
// frozen contract pins 1..60 chars.
const (
	minListName     = 1
	maxListName     = 60
	maxMembershipID = 100 // cap on ?ids= CSV length for the picker hydration
)

// watchlistJSON is the wire shape for a list in the overview / create /
// rename responses. itemCount is omitted-as-zero is fine; createdAt is
// RFC3339 per the contract. counts is the overview's, with include=counts
// alone.
type watchlistJSON struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	ItemCount int         `json:"itemCount"`
	IsDefault bool        `json:"isDefault"`
	CreatedAt string      `json:"createdAt"`
	Counts    *kindCounts `json:"counts,omitempty"`
}

// A list by kind: its movies, and its shows. A movie goes with the movies;
// a series, an episode saved on its own and anything else a list could hold
// go with the shows, under series, so that nothing a list holds is left out
// - an id katalog-api serves no title of too, and, for the one answer, a
// title whose kind katalog-api did not say for it. The kind of each title is
// katalog-api's (katalog.Client.Kinds).
const (
	kindMovie  = "movie"
	kindSeries = "series"
)

// listKind is the part of a list a title of katalog-api's type goes in.
func listKind(typ string) string {
	if typ == kindMovie {
		return kindMovie
	}
	return kindSeries
}

// kindCounts is a list's titles counted by kind; the two add up to its
// itemCount.
type kindCounts struct {
	Movie  int `json:"movie"`
	Series int `json:"series"`
}

// kindsUnavailable answers a list whose titles' kinds katalog-api could say
// none of: 502, and nothing of the list.
func kindsUnavailable(w http.ResponseWriter, err error) {
	log.Printf("chino-api: the kinds of a list's titles could not be asked: %v", err)
	http.Error(w, "catalog unavailable", http.StatusBadGateway)
}

// kindsOf is the kind katalog-api gives each of ids (katalog.Client.Kinds),
// for the viewer of r. One it did not say is not in it, and goes with the
// shows (listKind); where it said none, kindsOf answers 502 and is false.
func kindsOf(w http.ResponseWriter, r *http.Request, kc *katalog.Client, ids []string) (map[string]string, bool) {
	kinds, err := kc.Kinds(r.Context(), bearerFrom(r), ids)
	if err != nil {
		kindsUnavailable(w, err)
		return nil, false
	}
	unsaid := map[string]bool{}
	for _, id := range ids {
		if _, said := kinds[id]; !said {
			unsaid[id] = true
		}
	}
	if len(unsaid) > 0 {
		log.Printf("chino-api: katalog-api did not say the kind of %d of a list's titles; they go with the shows in this answer", len(unsaid))
	}
	return kinds, true
}

// once reports whether r's query parses and gives name at most once. A query
// that does not parse (a pair with a semicolon, a bad escape), which
// r.URL.Query would answer without the pairs it cannot read, once answers
// "bad query"; one that gives name more often, refused: each a 400 of plain
// text.
func once(w http.ResponseWriter, r *http.Request, name, refused string) bool {
	q, err := url.ParseQuery(r.URL.RawQuery)
	switch {
	case err != nil:
		http.Error(w, "bad query", http.StatusBadRequest)
	case len(q[name]) > 1:
		http.Error(w, refused, http.StatusBadRequest)
	default:
		return true
	}
	return false
}

// includes reports whether r's include parameter, a comma-separated list,
// names token. A token it does not know is passed over, as katalog-api's
// include passes one over.
func includes(r *http.Request, token string) bool {
	for _, t := range strings.Split(r.URL.Query().Get("include"), ",") {
		if strings.TrimSpace(t) == token {
			return true
		}
	}
	return false
}

func toWatchlistJSON(r store.WatchlistRow) watchlistJSON {
	return watchlistJSON{
		ID:        r.ID,
		Name:      r.Name,
		ItemCount: r.ItemCount,
		IsDefault: r.IsDefault,
		CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// validListName trims and bounds a proposed name. Returns the cleaned
// name and ok=false when it's empty or too long.
func validListName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if len(name) < minListName || len(name) > maxListName {
		return "", false
	}
	return name, true
}

// listError maps a store sentinel to the right status. Returns true when
// it handled (wrote) the error.
func listError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrListNotFound):
		http.Error(w, "list not found", http.StatusNotFound)
	case errors.Is(err, store.ErrListLimit):
		http.Error(w, "too many lists", http.StatusConflict)
	case errors.Is(err, store.ErrDuplicateName):
		http.Error(w, "name exists", http.StatusConflict)
	case errors.Is(err, store.ErrCannotDeleteDefault):
		http.Error(w, "cannot delete default", http.StatusConflict)
	default:
		http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
	}
	return true
}

// listWatchlists -> GET /me/watchlists. Default list first, then others
// by createdAt asc (ordering enforced in the store query). A capped viewer's
// itemCount counts the titles of the list its cap allows. With
// include=counts, each list has counts, its titles by kind:
// {"movie": 4, "series": 3}. include given twice is 400.
func listWatchlists(st *store.Store, g gate, kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		if !once(w, r, "include", "include is given once") {
			return
		}
		rows, err := st.ListWatchlists(r.Context(), userID)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		_, capped := auth.MaxRatingFromContext(r.Context())
		withCounts := includes(r, "counts")
		var counts map[string]*kindCounts
		if capped || withCounts {
			// Every list's titles, from one read, so that a list's count
			// and its counts by kind are of the same titles.
			held, err := st.WatchlistItemsByList(r.Context(), userID)
			if err != nil {
				http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
				return
			}
			var all []string
			for _, row := range rows {
				all = append(all, held[row.ID]...)
			}
			allowed, err := g.visibleIDs(r.Context(), all)
			if err != nil {
				catalogUnavailable(w, err)
				return
			}
			var kinds map[string]string
			if withCounts {
				var ok bool
				if kinds, ok = kindsOf(w, r, kc, allowed); !ok {
					return
				}
				counts = make(map[string]*kindCounts, len(rows))
			}
			shown := make(map[string]bool, len(allowed))
			for _, id := range allowed {
				shown[id] = true
			}
			for i := range rows {
				c := &kindCounts{}
				for _, id := range held[rows[i].ID] {
					if !shown[id] {
						continue
					}
					if listKind(kinds[id]) == kindMovie {
						c.Movie++
					} else {
						c.Series++
					}
				}
				rows[i].ItemCount = c.Movie + c.Series
				if withCounts {
					counts[rows[i].ID] = c
				}
			}
		}
		lists := make([]watchlistJSON, 0, len(rows))
		for _, row := range rows {
			l := toWatchlistJSON(row)
			l.Counts = counts[row.ID]
			lists = append(lists, l)
		}
		writeJSON(w, http.StatusOK, map[string]any{"lists": lists})
	}
}

// createWatchlist -> POST /me/watchlists {"name"}. 201 with the new list.
func createWatchlist(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		name, ok := validListName(body.Name)
		if !ok {
			http.Error(w, "name must be 1..60 chars", http.StatusBadRequest)
			return
		}
		row, err := st.CreateWatchlist(r.Context(), userID, name)
		if listError(w, err) {
			return
		}
		writeJSON(w, http.StatusCreated, toWatchlistJSON(row))
	}
}

// renameWatchlist -> PATCH /me/watchlists/{listId} {"name"}. Renaming the
// default keeps isDefault=true (store leaves the flag alone).
func renameWatchlist(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		listID := chi.URLParam(r, "listId")
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		name, ok := validListName(body.Name)
		if !ok {
			http.Error(w, "name must be 1..60 chars", http.StatusBadRequest)
			return
		}
		row, err := st.RenameWatchlist(r.Context(), userID, listID, name)
		if listError(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, toWatchlistJSON(row))
	}
}

// deleteWatchlist -> DELETE /me/watchlists/{listId}. 204; cascades items.
func deleteWatchlist(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		listID := chi.URLParam(r, "listId")
		if err := st.DeleteWatchlist(r.Context(), userID, listID); listError(w, err) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// getWatchlist -> GET /me/watchlists/{listId}. Returns the list metadata
// plus its item ids (newest-added first), a capped viewer's those its cap
// allows. 404 if not the caller's. With kind=movie, its movies alone; with
// kind=series, everything else (its series, its episodes, any other kind).
// Another kind, or kind given twice, is 400.
func getWatchlist(st *store.Store, g gate, kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		if !once(w, r, "kind", "kind is movie or series") {
			return
		}
		kind := r.URL.Query().Get("kind")
		if kind != "" && kind != kindMovie && kind != kindSeries {
			http.Error(w, "kind is movie or series", http.StatusBadRequest)
			return
		}
		listID := chi.URLParam(r, "listId")
		row, err := st.GetWatchlist(r.Context(), userID, listID)
		if listError(w, err) {
			return
		}
		ids, err := st.GetWatchlistItems(r.Context(), listID)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if ids, err = g.visibleIDs(r.Context(), ids); err != nil {
			catalogUnavailable(w, err)
			return
		}
		if kind != "" {
			kinds, ok := kindsOf(w, r, kc, ids)
			if !ok {
				return
			}
			ids = slices.DeleteFunc(ids, func(id string) bool { return listKind(kinds[id]) != kind })
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":        row.ID,
			"name":      row.Name,
			"isDefault": row.IsDefault,
			"items":     idsOrEmpty(ids),
		})
	}
}

// setWatchlistItem handles PUT/DELETE /me/watchlists/{listId}/items/{itemId}.
// Both idempotent, 204; 404 if the list isn't the caller's.
func setWatchlistItem(st *store.Store, add bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		listID := chi.URLParam(r, "listId")
		itemID := chi.URLParam(r, "itemId")
		if itemID == "" {
			http.Error(w, "missing item id", http.StatusBadRequest)
			return
		}
		var err error
		if add {
			err = st.AddWatchlistItem(r.Context(), userID, listID, itemID)
		} else {
			err = st.RemoveWatchlistItem(r.Context(), userID, listID, itemID)
		}
		if listError(w, err) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// watchlistMemberships -> GET /me/watchlists/memberships?ids=a,b,c.
// Returns { memberships: { itemId: [listId,...] } } for the caller's
// lists. Items in no list may be omitted. Drives the picker checkmarks
// and the card "saved" badge. A capped viewer's leave out the titles its
// cap does not allow.
func watchlistMemberships(st *store.Store, g gate) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		ids, err := g.visibleIDs(r.Context(), parseIDsCSV(r.URL.Query().Get("ids"), maxMembershipID))
		if err != nil {
			catalogUnavailable(w, err)
			return
		}
		m, err := st.ListMemberships(r.Context(), userID, ids)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if m == nil {
			m = map[string][]string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"memberships": m})
	}
}

// parseIDsCSV splits a comma-separated id list, trims blanks, dedups, and
// caps at max so a single request can't pull an unbounded membership
// scan.
func parseIDsCSV(raw string, max int) []string {
	if raw == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		if len(out) >= max {
			break
		}
	}
	return out
}

// --- Back-compat: /me/watchlist now maps to the default list ---------------
//
// These keep the exact response shapes the already-installed mobile/TV
// builds expect ({items:[...]} on GET, 204 on PUT/DELETE) but operate on
// the user's default list resolved via EnsureDefaultList. The default
// list is created lazily here so a fresh user's first add still works.

// defaultListGet -> GET /me/watchlist == default list's items, a capped
// viewer's those its cap allows.
func defaultListGet(st *store.Store, g gate) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		listID, err := st.EnsureDefaultList(r.Context(), userID)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		ids, err := st.GetWatchlistItems(r.Context(), listID)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if ids, err = g.visibleIDs(r.Context(), ids); err != nil {
			catalogUnavailable(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": idsOrEmpty(ids)})
	}
}

// defaultListSet -> PUT/DELETE /me/watchlist/{id} == add/remove on the
// default list. 204 either way.
func defaultListSet(st *store.Store, add bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		itemID := chi.URLParam(r, "id")
		if itemID == "" {
			http.Error(w, "missing item id", http.StatusBadRequest)
			return
		}
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		listID, err := st.EnsureDefaultList(r.Context(), userID)
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if add {
			err = st.AddWatchlistItem(r.Context(), userID, listID, itemID)
		} else {
			err = st.RemoveWatchlistItem(r.Context(), userID, listID, itemID)
		}
		if err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
