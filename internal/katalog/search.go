package katalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// SearchSeries is the series of an episode a search found, as katalog-api
// sends it (its id and title, and whether the series matched the query by its
// own title in the same search), with the series' poster, synthesised as an
// item's poster_url is.
type SearchSeries struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Matched   bool   `json:"matched"`
	PosterURL string `json:"poster_url,omitempty"`
}

// SearchResult is one search of the catalog: its titles, ranked together by
// how they match the query, best first, each with how it matched and an
// episode with its series; and the people whose names match it, ranked apart.
type SearchResult struct {
	Items  []Item   `json:"items"`
	People []Person `json:"people"`
}

// SearchOpts is a search: the query, the types of title to look at (movie,
// series, episode; katalog-api looks at all of them when none is named), how
// many titles and how many people (0 looks for none).
type SearchOpts struct {
	Query  string
	Types  []string
	Limit  int
	People int
}

// ErrNoSearch is a katalog-api without /api/v1/search, older than it.
var ErrNoSearch = errors.New("katalog: no search")

// upstreamSearch is katalog-api's search as it sends it.
type upstreamSearch struct {
	Items []struct {
		upstreamItem
		Match  string        `json:"match"`
		Series *SearchSeries `json:"series"`
	} `json:"items"`
	People []Person `json:"people"`
}

// Search runs katalog-api's search (/api/v1/search) for the viewer of ctx.
// Items come with their poster and backdrop URLs, the series of an episode
// with its poster's, and people with their profile_url, as the lists and
// /people send them. ErrNoSearch when katalog-api has no search.
func (c *Client) Search(ctx context.Context, bearer string, opts SearchOpts) (SearchResult, error) {
	v := url.Values{"q": {opts.Query}}
	if len(opts.Types) > 0 {
		v.Set("types", strings.Join(opts.Types, ","))
	}
	if opts.Limit > 0 {
		v.Set("limit", strconv.Itoa(opts.Limit))
	}
	v.Set("people", strconv.Itoa(max(opts.People, 0)))
	req, err := c.newRequest(ctx, "/api/v1/search", v, bearer)
	if err != nil {
		return SearchResult{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return SearchResult{}, fmt.Errorf("katalog search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return SearchResult{}, ErrNoSearch
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return SearchResult{}, fmt.Errorf("katalog search: %d %s", resp.StatusCode, string(body))
	}
	var raw upstreamSearch
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return SearchResult{}, fmt.Errorf("decode katalog search: %w", err)
	}
	out := SearchResult{Items: make([]Item, 0, len(raw.Items)), People: make([]Person, 0, len(raw.People))}
	for _, u := range raw.Items {
		it := u.upstreamItem.toItem()
		it.Match = u.Match
		if s := u.Series; s != nil && s.ID != "" {
			it.Series = &SearchSeries{ID: s.ID, Title: s.Title, Matched: s.Matched,
				PosterURL: "/api/v1/items/" + url.PathEscape(s.ID) + "/poster"}
		}
		out.Items = append(out.Items, it)
	}
	for _, p := range raw.People {
		out.People = append(out.People, p.withProfileURL())
	}
	return out, nil
}
