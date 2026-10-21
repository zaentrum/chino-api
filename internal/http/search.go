package http

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/katalog"
	"github.com/zaentrum/chino-api/internal/store"
)

// Search limits: titles and people a search answers by default and at most.
const (
	searchLimit     = 30
	maxSearchLimit  = 100
	searchPeopleN   = 12
	maxSearchPeople = 50
)

// search is one search of the catalog, katalog-api's /api/v1/search: its
// titles (films, series and episodes) ranked together by how they match the
// query, best first, each with how it matched and an episode with its series;
// and the people whose names match it, ranked apart. ?q= is the query (blank:
// nothing found), ?types= the types of title to look at, comma separated
// (movie, series, episode; all of them when it names none), ?limit= how many
// titles (default 30, at most 100), ?people= how many people (default 12, at
// most 50; 0 looks for none). The titles carry their poster URLs and the
// viewer's watched_at, as the lists do; an episode's series its poster URL;
// a person their profile_url.
//
// A katalog-api without the search (older than it) is searched as the
// clients searched before: each type's list of the query, one type after the
// other, and the people by /people.
func search(st *store.Store, kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		opts := katalog.SearchOpts{Query: strings.TrimSpace(q.Get("q")), Limit: searchLimit, People: searchPeopleN}
		for _, v := range q["types"] {
			for _, t := range strings.Split(v, ",") {
				if t = strings.TrimSpace(t); t != "" {
					opts.Types = append(opts.Types, t)
				}
			}
		}
		if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
			opts.Limit = min(n, maxSearchLimit)
		}
		if n, err := strconv.Atoi(q.Get("people")); err == nil && n >= 0 {
			opts.People = min(n, maxSearchPeople)
		}
		res := katalog.SearchResult{Items: []katalog.Item{}, People: []katalog.Person{}}
		if opts.Query != "" {
			var err error
			res, err = kc.Search(r.Context(), bearerFrom(r), opts)
			if errors.Is(err, katalog.ErrNoSearch) {
				res, err = searchAsBefore(r.Context(), kc, bearerFrom(r), opts)
			}
			if err != nil {
				// As listItems: the catalog is upstream of us and it failed.
				log.Printf("chino-api: katalog search failed (q=%q): %v", opts.Query, err)
				writeJSON(w, http.StatusBadGateway, map[string]any{
					"product": "chino",
					"error":   "catalog unavailable",
					"detail":  err.Error(),
				})
				return
			}
		}
		userID, _ := auth.SubjectFromContext(r.Context())
		stampWatchedSlice(r.Context(), st, userID, res.Items)
		writeJSON(w, http.StatusOK, map[string]any{
			"product": "chino",
			"items":   res.Items,
			"people":  res.People,
			"source":  "katalog",
		})
	}
}

// searchAsBefore is a search of a katalog-api that has none: the list of the
// query of each type asked for (films, series and episodes when none is),
// each ranked by katalog-api and of up to the limit, one type after the
// other, as the clients asked for them; and the people by /people, best
// effort.
func searchAsBefore(ctx context.Context, kc *katalog.Client, bearer string, opts katalog.SearchOpts) (katalog.SearchResult, error) {
	types := opts.Types
	if len(types) == 0 {
		types = []string{"movie", "series", "episode"}
	}
	out := katalog.SearchResult{Items: []katalog.Item{}, People: []katalog.Person{}}
	for _, t := range types {
		var (
			items []katalog.Item
			err   error
		)
		switch t {
		case "movie":
			items, err = kc.ListMoviesFiltered(ctx, bearer, opts.Query, opts.Limit, 0, nil)
		case "series":
			items, err = kc.ListSeriesFiltered(ctx, bearer, opts.Query, opts.Limit, 0, nil)
		case "episode":
			items, err = kc.ListEpisodesFiltered(ctx, bearer, opts.Query, opts.Limit, 0, nil)
		default:
			continue
		}
		if err != nil {
			return katalog.SearchResult{}, err
		}
		out.Items = append(out.Items, items...)
	}
	if opts.People > 0 {
		if people, err := kc.SearchPeople(ctx, bearer, opts.Query, opts.People); err == nil {
			out.People = people
		}
	}
	return out, nil
}
