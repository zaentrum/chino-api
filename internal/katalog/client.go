// Package katalog is a thin client over the chino read API surface.
// Today this is `katalog-api` (Go, read-only, ADR-011 split — cloud_katalog_ro
// Postgres role); writes from chino-api still go to katalog-manager-api but
// chino-api itself is purely a read consumer, so we never call the write side.
//
// Wire shape: clean REST (`/api/v1/...`), snake_case JSON, paginated envelope
// `{ items, total, limit, offset }`. No OData $filter / $expand magic; query
// parameters are simple (`q`, `year_min`, `year_max`, `rating_min`, `genre`,
// `sort`, `limit`, `offset`). The legacy `/odata/v4/katalog-admin/...` path
// is dead and removed.
package katalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/redact"
)

type Client struct {
	// BaseURL → katalog-api (Go read-only). Owns /api/v1/items, /movies,
	// /series, /episodes, /albums, /genres, /items/{id}, /series/{id}/episodes,
	// /items/{id}/segments, /items/{id}/asset. All snake_case JSON, no OData.
	BaseURL string
	// StreamBaseURL → chino-stream (HLS + trickplay + per-item /play/info).
	// ProxyStream routes /api/play/... here.
	StreamBaseURL string
	// ArtworkBaseURL → katalog-manager-api (CAP Java). Artwork lives in
	// itemartworkdata which is owned by the write surface; katalog-api
	// hasn't grown an artwork endpoint yet. When it does, flip this to
	// match BaseURL and delete the field.
	ArtworkBaseURL string

	// HTTP is for short-lived JSON/metadata calls (GetItem, ListMovies,
	// etc) — capped to 20 s so a misbehaving katalog upstream can't hang
	// the API.
	HTTP *http.Client
	// HTTPStream is used by ProxyStream for /play and /play/subtitles.
	// No total-time Timeout because video transcodes run for the whole
	// movie (potentially hours). Termination happens via
	// context.WithCancel from the inbound r.Context() — client
	// disconnect cancels the upstream call cleanly.
	HTTPStream *http.Client

	// visible keeps katalog-api's answers of what a capped viewer may be
	// served, and of the titles sidecar subtitles belong to (ratings.go).
	visible answers
}

// New wires the read client. Callers that also need streaming +
// artwork must populate the other base URLs afterwards (see main.go).
func New(baseURL string) *Client {
	return &Client{
		BaseURL:    baseURL,
		HTTP:       &http.Client{Timeout: 20 * time.Second},
		HTTPStream: &http.Client{Timeout: 0},
	}
}

// Item is the projection chino-web consumes. Mirrors katalog-api's wire
// shape (snake_case) with a couple of chino-api-only fields appended:
//   - PosterURL / BackdropURL are synthesised on the way out so the
//     frontend hits chino-api's artwork proxy (cookie-gated) instead of
//     katalog-api directly.
//   - WatchedAt is filled in by chino-api after a per-user lookup
//     against its own progress table; never comes from katalog-api.
type Item struct {
	ID          string  `json:"id"`
	Type        string  `json:"type,omitempty"`
	Title       string  `json:"title"`
	Year        *int    `json:"year,omitempty"`
	Rating      float64 `json:"rating,omitempty"`
	Description string  `json:"description,omitempty"`
	Tagline     string  `json:"tagline,omitempty"`
	DurationMs  int64   `json:"duration_ms,omitempty"`
	PosterURL   string  `json:"poster_url,omitempty"`
	BackdropURL string  `json:"backdrop_url,omitempty"`

	// Episode coordinates, only set for type=episode.
	SeasonNumber  *int   `json:"season_number,omitempty"`
	EpisodeNumber *int   `json:"episode_number,omitempty"`
	ParentID      string `json:"parent_id,omitempty"`

	// MinAge is the age a viewer must be to be served the title (an admin's
	// rating, else an episode's series', else its certification's); nil
	// when nothing rates it, and a pointer so 0 survives the round-trip.
	// Certification and CertificationCountry are the certification the age
	// comes from as TMDB gives it ("12" in "DE", "PG-13" in "US"), empty
	// when an admin rated the title or nothing did. As katalog-api sends
	// them.
	MinAge               *int   `json:"min_age,omitempty"`
	Certification        string `json:"certification,omitempty"`
	CertificationCountry string `json:"certification_country,omitempty"`

	// Optional rich associations populated by GetItemDetail.
	Genres    []string    `json:"genres,omitempty"`
	Cast      []CastEntry `json:"cast,omitempty"`
	Subtitles []Subtitle  `json:"subtitles,omitempty"`
	Trailers  []Trailer   `json:"trailers,omitempty"`
	Extras    []Extra     `json:"extras,omitempty"`
	Segments  *SegSummary `json:"segments,omitempty"`

	// WatchedAt is set by chino-api (not katalog) when the current user
	// has marked this item watched. Nil means unwatched. Lives on Item
	// because chino-api enriches lists in-place before returning JSON.
	WatchedAt *time.Time `json:"watched_at,omitempty"`

	// Roles is set on a person's filmography only: the person's roles on
	// this item, in katalog-api's credit order.
	Roles []string `json:"roles,omitempty"`
}

// CastEntry is one credit, as katalog-api sends it. Role is an open
// vocabulary (actor, creator, director, writer, producer, composer,
// cinematographer, editor, or any other token); a client shows the roles it
// knows.
type CastEntry struct {
	// PersonID deep-links a cast chip to /people/{id} (filmography).
	PersonID string `json:"person_id,omitempty"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	// Job is the job within the role ("Screenplay"), Character the part an
	// actor plays, Order the billing order within the role (0 first — a
	// pointer, so 0 survives the round-trip) and EpisodeCount how many
	// episodes of a series the credit covers. Each is omitted when unknown.
	Job          string `json:"job,omitempty"`
	Character    string `json:"character,omitempty"`
	Order        *int   `json:"order,omitempty"`
	EpisodeCount *int   `json:"episode_count,omitempty"`
}

type Subtitle struct {
	ID      string `json:"id"`
	Lang    string `json:"lang"`
	Label   string `json:"label,omitempty"`
	Format  string `json:"format,omitempty"`
	Default bool   `json:"default,omitempty"`
	// Forced is a subtitle a player shows by itself for the language it is in
	// (katalog-api's forced, katalog-manager's isforced).
	Forced bool `json:"forced,omitempty"`
	// URL is synthesised by chino-api's subtitles handler, NOT returned
	// by katalog-api. Points at /api/v1/play/subs/<id>.vtt which the
	// chino-api proxy forwards to chino-stream → file on disk.
	URL string `json:"url,omitempty"`
}

// Trailer is one of the item's links to an online video, as katalog-api
// sends it. Installed clients read every entry as such a link: mobile and TV
// decode url as required (an entry without one fails the whole item), and
// every client opens a trailer's url outside the app. So nothing but these
// links goes here; a trailer this server plays is an Extra.
type Trailer struct {
	Site       string `json:"site,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	URL        string `json:"url"`
	Title      string `json:"title,omitempty"`
}

// Extra is one of a movie's or a series' extras that plays: a trailer, a
// teaser, a featurette, … that is a file of its own, packaged for streaming
// apart from the title (katalog-api's include=extras), in the order a viewer
// sees them. ID, Kind (trailer, teaser, featurette, behind-the-scenes,
// making-of, deleted-scene, interview, gag-reel, short, other), Title,
// Language (BCP 47) and DurationMs are katalog-api's; SeasonNumber is set on
// a series' extra of one season (0 the specials).
//
// PlayPath is synthesised by chino-api, as PosterURL is: the extra's HLS
// master, /api/v1/items/{id}/extras/{extraId}/play/master.m3u8, which a
// client asks for as it asks for a title's master (?stream=<token>,
// &caps=). Local is always true: an extra plays from this server, where a
// Trailer is a link to elsewhere.
type Extra struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Title        string `json:"title"`
	Language     string `json:"language,omitempty"`
	DurationMs   int64  `json:"duration_ms,omitempty"`
	SeasonNumber *int   `json:"season_number,omitempty"`
	Local        bool   `json:"local"`
	PlayPath     string `json:"play_path"`
}

// extraPlayPath is the path of the HLS master of the extra extraID of the
// title itemID on chino-api.
func extraPlayPath(itemID, extraID string) string {
	return "/api/v1/items/" + url.PathEscape(itemID) + "/extras/" + url.PathEscape(extraID) + "/play/master.m3u8"
}

type SegSummary struct {
	HasIntro   bool `json:"has_intro"`
	HasCredits bool `json:"has_credits"`
	HasRecap   bool `json:"has_recap"`
	Count      int  `json:"count"`
}

// upstreamItem mirrors katalog-api's wire shape verbatim. Kept separate
// from Item so the chino-api-only fields (PosterURL synthesis, WatchedAt
// enrichment) don't accidentally leak into the upstream parser.
type upstreamItem struct {
	ID            string  `json:"id"`
	Type          string  `json:"type"`
	Title         string  `json:"title"`
	SortTitle     string  `json:"sort_title,omitempty"`
	Year          *int    `json:"year"`
	Rating        float64 `json:"rating"`
	Description   string  `json:"description"`
	Tagline       string  `json:"tagline"`
	DurationMs    int64   `json:"duration_ms"`
	SeasonNumber  *int    `json:"season_number"`
	EpisodeNumber *int    `json:"episode_number"`
	ParentID      string  `json:"parent_id"`

	MinAge               *int   `json:"min_age"`
	Certification        string `json:"certification"`
	CertificationCountry string `json:"certification_country"`

	// Populated only by GET /items/{id}?include=…; absent on list responses.
	Genres    []string                `json:"genres,omitempty"`
	Cast      []CastEntry             `json:"cast,omitempty"`
	Subtitles []Subtitle              `json:"subtitles,omitempty"`
	Trailers  []Trailer               `json:"trailers,omitempty"`
	Extras    []upstreamExtra         `json:"extras,omitempty"`
	Segments  *upstreamSegmentSummary `json:"segments,omitempty"`

	// Populated only on GET /people/{id}'s filmography.
	Roles []string `json:"roles,omitempty"`
}

// upstreamExtra is one of an item's extras as katalog-api sends it; the list
// is absent when the item has none, and from a katalog-api or a catalog that
// knows no extras yet.
type upstreamExtra struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Title        string `json:"title"`
	Language     string `json:"language"`
	DurationMs   int64  `json:"duration_ms"`
	SeasonNumber *int   `json:"season_number"`
}

type upstreamSegmentSummary struct {
	Count      int  `json:"count"`
	HasIntro   bool `json:"has_intro"`
	HasCredits bool `json:"has_credits"`
	HasRecap   bool `json:"has_recap"`
}

func (u upstreamItem) toItem() Item {
	it := Item{
		ID:            u.ID,
		Type:          u.Type,
		Title:         u.Title,
		Year:          u.Year,
		Rating:        u.Rating,
		Description:   u.Description,
		Tagline:       u.Tagline,
		DurationMs:    u.DurationMs,
		SeasonNumber:  u.SeasonNumber,
		EpisodeNumber: u.EpisodeNumber,
		ParentID:      u.ParentID,
		MinAge:        u.MinAge,
		PosterURL:     "/api/v1/items/" + u.ID + "/poster",
		BackdropURL:   "/api/v1/items/" + u.ID + "/backdrop",
		Genres:        u.Genres,
		Cast:          u.Cast,
		Subtitles:     u.Subtitles,
		Trailers:      u.Trailers,
		Roles:         u.Roles,

		Certification:        u.Certification,
		CertificationCountry: u.CertificationCountry,
	}
	if u.Segments != nil {
		it.Segments = &SegSummary{
			Count:      u.Segments.Count,
			HasIntro:   u.Segments.HasIntro,
			HasCredits: u.Segments.HasCredits,
			HasRecap:   u.Segments.HasRecap,
		}
	}
	// Each extra plays at its play path, built as the poster's URL is.
	for _, e := range u.Extras {
		it.Extras = append(it.Extras, Extra{
			ID:           e.ID,
			Kind:         e.Kind,
			Title:        e.Title,
			Language:     e.Language,
			DurationMs:   e.DurationMs,
			SeasonNumber: e.SeasonNumber,
			Local:        true,
			PlayPath:     extraPlayPath(u.ID, e.ID),
		})
	}
	// The cast passes through as katalog-api orders and caps it: role by
	// role, billing order within a role, at most 20 actors and 10 people
	// of every other role. Trimming it here (it kept the first 8, actors
	// first) dropped every crew credit of a title with eight actors.
	return it
}

// listResult is the envelope every paginated list endpoint returns.
type listResult struct {
	Items  []upstreamItem `json:"items"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

// ListMovies / ListSeries / ListAlbums — convenience wrappers around the
// per-type list endpoints. Bearer is forwarded so katalog-api's
// resource-server check accepts the call.
func (c *Client) ListMovies(ctx context.Context, bearer, q string, limit int) ([]Item, error) {
	return c.listByType(ctx, bearer, "movies", q, limit, 0, nil)
}
func (c *Client) ListSeries(ctx context.Context, bearer, q string, limit int) ([]Item, error) {
	return c.listByType(ctx, bearer, "series", q, limit, 0, nil)
}
func (c *Client) ListAlbums(ctx context.Context, bearer, q string, limit int) ([]Item, error) {
	return c.listByType(ctx, bearer, "albums", q, limit, 0, nil)
}

// ListMoviesFiltered / ListSeriesFiltered expose the browse filter bar:
// optional year-range and rating, plus offset paging.
func (c *Client) ListMoviesFiltered(ctx context.Context, bearer, q string, limit, offset int, extra url.Values) ([]Item, error) {
	return c.listByType(ctx, bearer, "movies", q, limit, offset, extra)
}
func (c *Client) ListSeriesFiltered(ctx context.Context, bearer, q string, limit, offset int, extra url.Values) ([]Item, error) {
	return c.listByType(ctx, bearer, "series", q, limit, offset, extra)
}

// ListEpisodesFiltered lists episodes across all series (katalog's flat
// /episodes entity set), newest-first when sort=newest. Used by the Zap
// pool so shows — the bulk of the packaged catalogue — appear in the feed.
func (c *Client) ListEpisodesFiltered(ctx context.Context, bearer, q string, limit, offset int, extra url.Values) ([]Item, error) {
	return c.listByType(ctx, bearer, "episodes", q, limit, offset, extra)
}

// ListAll is the cross-type browse used by chino-web's global search.
func (c *Client) ListAll(ctx context.Context, bearer, q string, limit int) ([]Item, error) {
	return c.listAt(ctx, bearer, "/api/v1/items", q, limit, 0, nil)
}

func (c *Client) listByType(ctx context.Context, bearer, kind, q string, limit, offset int, extra url.Values) ([]Item, error) {
	return c.listAt(ctx, bearer, "/api/v1/"+kind, q, limit, offset, extra)
}

// newRequest is a GET of katalog-api's path with the query q (may be nil)
// for the viewer of ctx: with its bearer, and with its rating cap as
// max_rating (auth.MaxRatingFromContext), so that katalog-api serves a capped
// viewer only what the cap allows. Every catalog request goes through it.
func (c *Client) newRequest(ctx context.Context, path string, q url.Values, bearer string) (*http.Request, error) {
	if age, capped := auth.MaxRatingFromContext(ctx); capped {
		if q == nil {
			q = url.Values{}
		}
		q.Set("max_rating", strconv.Itoa(age))
	}
	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return req, nil
}

func (c *Client) listAt(ctx context.Context, bearer, path, q string, limit, offset int, extra url.Values) ([]Item, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	v := url.Values{}
	v.Set("limit", strconv.Itoa(limit))
	if offset > 0 {
		v.Set("offset", strconv.Itoa(offset))
	}
	if q != "" {
		v.Set("q", q)
	}
	for k, vals := range extra {
		for _, val := range vals {
			v.Set(k, val)
		}
	}
	req, err := c.newRequest(ctx, path, v, bearer)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("katalog request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("katalog %s: %d %s", path, resp.StatusCode, string(body))
	}
	var raw listResult
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode katalog: %w", err)
	}
	out := make([]Item, 0, len(raw.Items))
	for _, u := range raw.Items {
		out = append(out, u.toItem())
	}
	return out, nil
}

// GetItem returns a single item by id, no associations. 404 → (nil, nil).
func (c *Client) GetItem(ctx context.Context, bearer, id string) (*Item, error) {
	return c.getItem(ctx, bearer, id, "")
}

// GetItemDetail returns the item plus its rich associations
// (genres, cast, subtitles, trailers, extras, segments summary). Single
// REST call — the old four-set OData hop is gone because katalog-api keys
// off the unified items table. A katalog-api that knows no extras ignores
// the token, and the item has none.
func (c *Client) GetItemDetail(ctx context.Context, bearer, id string) (*Item, error) {
	return c.getItem(ctx, bearer, id, "genres,cast,subtitles,trailers,extras,segments")
}

func (c *Client) getItem(ctx context.Context, bearer, id, include string) (*Item, error) {
	var q url.Values
	if include != "" {
		q = url.Values{"include": {include}}
	}
	req, err := c.newRequest(ctx, "/api/v1/items/"+url.PathEscape(id), q, bearer)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("katalog request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("katalog item(%s): %d %s", id, resp.StatusCode, string(body))
	}
	var u2 upstreamItem
	if err := json.NewDecoder(resp.Body).Decode(&u2); err != nil {
		return nil, fmt.Errorf("decode katalog: %w", err)
	}
	if u2.ID == "" {
		return nil, nil
	}
	it := u2.toItem()
	return &it, nil
}

// ListGenres returns the catalogue-wide genre list, sorted alphabetically.
// Used by chino-web's browse filter chips so the picker shows real values.
func (c *Client) ListGenres(ctx context.Context, bearer string) ([]string, error) {
	req, err := c.newRequest(ctx, "/api/v1/genres", nil, bearer)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("katalog genres: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("katalog genres: %d", resp.StatusCode)
	}
	var raw struct {
		Genres []string `json:"genres"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	return raw.Genres, nil
}

// ListSeriesEpisodes returns every episode under a given series, ordered
// by season+episode. Used to render the Series detail page.
func (c *Client) ListSeriesEpisodes(ctx context.Context, bearer, seriesID string) ([]Item, error) {
	req, err := c.newRequest(ctx, "/api/v1/series/"+url.PathEscape(seriesID)+"/episodes", nil, bearer)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("katalog episodes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("katalog episodes: %d %s", resp.StatusCode, string(body))
	}
	var raw struct {
		Items []upstreamItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(raw.Items))
	for _, r := range raw.Items {
		out = append(out, r.toItem())
	}
	return out, nil
}

// ListSimilar returns up to N "more like this" items for the given
// source item, scored upstream by shared genre + cast (see
// katalog-api ListSimilar). Returns nil on a 404 — the source item
// id is unknown — so chino-api callers can degrade to a hidden row
// instead of surfacing an error to the UI.
func (c *Client) ListSimilar(ctx context.Context, bearer, itemID string, limit int) ([]Item, error) {
	if limit <= 0 || limit > 50 {
		limit = 12
	}
	req, err := c.newRequest(ctx, "/api/v1/items/"+url.PathEscape(itemID)+"/similar",
		url.Values{"limit": {strconv.Itoa(limit)}}, bearer)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("katalog similar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("katalog similar: %d %s", resp.StatusCode, string(body))
	}
	var raw struct {
		Items []upstreamItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(raw.Items))
	for _, r := range raw.Items {
		out = append(out, r.toItem())
	}
	return out, nil
}

// Segment is the raw row chino-web's player consumes to draw timeline
// markers and wire Skip-Intro / Skip-Credits buttons.
type Segment struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	StartMs    int64   `json:"start_ms"`
	EndMs      int64   `json:"end_ms"`
	Source     string  `json:"source,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Label      string  `json:"label,omitempty"`
}

// ListSegments returns the raw segments for an item, ordered by start time.
func (c *Client) ListSegments(ctx context.Context, bearer, itemID string) ([]Segment, error) {
	req, err := c.newRequest(ctx, "/api/v1/items/"+url.PathEscape(itemID)+"/segments", nil, bearer)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("katalog segments: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("katalog segments: %d %s", resp.StatusCode, string(body))
	}
	var raw struct {
		Items []Segment `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	return raw.Items, nil
}

// PlayInfoDurationMs returns the truth content duration (ffprobe-derived
// from the packaged manifest) for an item, by calling chino-stream's
// /api/play/{id}/info. Used to clamp stale TIDB segment end_ms values
// authored against TMDB-rounded runtimes longer than the actual file.
// Returns 0 on any soft failure — callers treat 0 as "skip clamping".
//
// Uses StreamBaseURL, not BaseURL, since /api/play lives on
// chino-stream — separated from katalog-api.
func (c *Client) PlayInfoDurationMs(ctx context.Context, bearer, itemID string) int64 {
	if c.StreamBaseURL == "" {
		return 0
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.StreamBaseURL+"/api/play/"+url.PathEscape(itemID)+"/info", nil)
	if err != nil {
		return 0
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return 0
	}
	var raw struct {
		DurationMs int64 `json:"duration_ms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return 0
	}
	return raw.DurationMs
}

// ProxyStream forwards the named upstream path to the appropriate
// service, copying Range + If-None-Match + Authorization + query string
// and streaming the response (status, headers such as ETag, body)
// verbatim. Routing is by path prefix because
// chino-api stitches three upstreams behind one client today:
//
//	/api/play/...    → StreamBaseURL  (chino-stream)
//	/api/artwork/... → ArtworkBaseURL (katalog-manager-api)
//	anything else    → BaseURL        (katalog-api read surface)
//
// When katalog-api grows an artwork endpoint, set ArtworkBaseURL to
// the same value as BaseURL and ProxyStream picks the new home
// automatically.
func (c *Client) ProxyStream(w http.ResponseWriter, r *http.Request, upstreamPath, bearer string) {
	base := c.BaseURL
	switch {
	case strings.HasPrefix(upstreamPath, "/api/play"):
		if c.StreamBaseURL != "" {
			base = c.StreamBaseURL
		}
	case strings.HasPrefix(upstreamPath, "/api/artwork"):
		if c.ArtworkBaseURL != "" {
			base = c.ArtworkBaseURL
		}
	}
	u := base + upstreamPath
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	// Forward the original HTTP method so POST endpoints (e.g. the
	// Zap pre-warm fire) flow through. Body is intentionally left
	// nil — the proxied play endpoints don't accept a body today;
	// when one does, this needs r.Body wired in with an io.LimitReader
	// to bound memory.
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u, nil)
	if err != nil {
		http.Error(w, "bad upstream url", http.StatusInternalServerError)
		return
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	// Range for byte ranges; If-None-Match so a client revalidating what it
	// cached (an artwork ETag) gets the upstream's 304, not the bytes again.
	for _, h := range []string{"Range", "If-None-Match"} {
		for _, v := range r.Header.Values(h) {
			req.Header.Add(h, v)
		}
	}
	resp, err := c.HTTPStream.Do(req)
	if err != nil {
		// The error quotes the upstream URL, query (credentials) included;
		// clients report error bodies in telemetry and bug reports.
		http.Error(w, "katalog upstream: "+redact.Text(err.Error()), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if k == "Authorization" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
