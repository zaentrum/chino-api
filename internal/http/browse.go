package http

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/zaentrum/chino-api/internal/katalog"
)

// buildBrowseFilter forwards chino-web's browse query parameters
// (year_min, year_max, rating_min, genre, sort) to the katalog-api
// `/api/v1/{movies,series}` endpoint. katalog-api expects native
// params (parseListOpts in katalog-api/internal/http/items.go), NOT
// OData $filter / $orderby — we tried OData once and katalog-api
// silently dropped the values, which is why Top Rated used to come
// back alphabetic. Defensive: bad values are dropped rather than
// erroring the request.
func buildBrowseFilter(r *http.Request) url.Values {
	out := url.Values{}
	q := r.URL.Query()

	if v := q.Get("year_min"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 1800 && n < 2200 {
			out.Set("year_min", strconv.Itoa(n))
		}
	}
	if v := q.Get("year_max"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 1800 && n < 2200 {
			out.Set("year_max", strconv.Itoa(n))
		}
	}
	if v := q.Get("rating_min"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 10 {
			out.Set("rating_min", strconv.FormatFloat(f, 'f', 1, 64))
		}
	}
	if v := q.Get("genre"); v != "" {
		out.Set("genre", v)
	}
	switch q.Get("sort") {
	case "rating", "year", "title", "newest":
		out.Set("sort", q.Get("sort"))
	}
	return out
}

// listGenres returns the catalogue's distinct genre names so the browse
// filter chips on chino-web reflect what's actually in the library.
func listGenres(kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		names, err := kc.ListGenres(r.Context(), bearerFrom(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"genres": names})
	}
}
