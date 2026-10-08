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
	"sync"
	"time"
)

// A capped viewer (auth.MaxRatingFromContext) is served only what its cap
// allows. katalog-api decides it on every catalog request chino-api makes
// (newRequest passes the cap). What chino-api serves itself, or proxies to a
// service that knows nothing of ratings (chino-stream's playback, the
// artwork, a sidecar subtitle file, the lists chino-api keeps), it asks
// katalog-api about: GET /api/v1/visible, which a request a stream token
// authorizes can ask too, as it takes no bearer.

// visibleTTL is how long an answer of what a cap allows is kept: a playback
// fetches a segment every few seconds, and asks once in this time. A title an
// admin rates anew is served or refused by its new rating within it.
const visibleTTL = 30 * time.Second

// subtitleTTL is how long the title a sidecar subtitle belongs to is kept: a
// subtitle does not move to another title.
const subtitleTTL = 10 * time.Minute

// visibleBatch is how many ids one /api/v1/visible request asks about
// (katalog-api takes 500).
const visibleBatch = 200

// maxAnswers bounds what is kept; once it is full, what expired goes, and if
// nothing has, everything.
const maxAnswers = 20000

// answers are katalog-api's answers, kept for a while.
type answers struct {
	mu       sync.Mutex
	visible  map[string]answer // "<cap>|<id>"
	subtitle map[string]answer // subtitle id -> its title's id (answer.item)
	file     map[string]answer // "<cap>|<id>" -> the episodes of its file (answer.items, files.go)
}

type answer struct {
	visible bool
	item    string
	items   []string
	until   time.Time
}

// keep stores a in m under k, making room first when m is full.
func keep(m map[string]answer, k string, a answer, now time.Time) {
	if len(m) >= maxAnswers {
		for key, old := range m {
			if !now.Before(old.until) {
				delete(m, key)
			}
		}
		if len(m) >= maxAnswers {
			clear(m)
		}
	}
	m[k] = a
}

// Visible is which of ids name a title a viewer capped at maxRating may be
// served, as katalog-api answers it: in the order given, each once; an id of
// no title is left out as one the cap leaves out. Answers are kept for
// visibleTTL, per cap and id.
func (c *Client) Visible(ctx context.Context, maxRating int, ids []string) ([]string, error) {
	now := time.Now()
	known := make(map[string]bool, len(ids))
	var ask []string
	c.visible.mu.Lock()
	if c.visible.visible == nil {
		c.visible.visible = map[string]answer{}
	}
	for _, id := range ids {
		if _, seen := known[id]; seen {
			continue
		}
		if a, ok := c.visible.visible[strconv.Itoa(maxRating)+"|"+id]; ok && now.Before(a.until) {
			known[id] = a.visible
			continue
		}
		known[id] = false
		ask = append(ask, id)
	}
	c.visible.mu.Unlock()

	for start := 0; start < len(ask); start += visibleBatch {
		batch := ask[start:min(start+visibleBatch, len(ask))]
		allowed, err := c.askVisible(ctx, maxRating, batch)
		if err != nil {
			return nil, err
		}
		c.visible.mu.Lock()
		for _, id := range batch {
			known[id] = allowed[id]
			keep(c.visible.visible, strconv.Itoa(maxRating)+"|"+id, answer{visible: allowed[id], until: now.Add(visibleTTL)}, now)
		}
		c.visible.mu.Unlock()
	}

	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if known[id] && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// VisibleOne reports whether a viewer capped at maxRating may be served the
// title id (Visible).
func (c *Client) VisibleOne(ctx context.Context, maxRating int, id string) (bool, error) {
	ids, err := c.Visible(ctx, maxRating, []string{id})
	return len(ids) == 1, err
}

// askVisible asks katalog-api which of ids a viewer capped at maxRating may
// be served.
func (c *Client) askVisible(ctx context.Context, maxRating int, ids []string) (map[string]bool, error) {
	u := c.BaseURL + "/api/v1/visible?" + url.Values{
		"ids": {strings.Join(ids, ",")}, "max_rating": {strconv.Itoa(maxRating)},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("katalog visible: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("katalog visible: %d %s", resp.StatusCode, string(body))
	}
	var raw struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("katalog visible: %w", err)
	}
	out := make(map[string]bool, len(raw.IDs))
	for _, id := range raw.IDs {
		out[id] = true
	}
	return out, nil
}

// SubtitleItem is the id of the title the sidecar subtitle subID belongs to,
// as katalog-api's GET /api/v1/subtitles/{id}/asset says it; "" when there is
// no such subtitle. Kept for subtitleTTL.
func (c *Client) SubtitleItem(ctx context.Context, subID string) (string, error) {
	now := time.Now()
	c.visible.mu.Lock()
	if c.visible.subtitle == nil {
		c.visible.subtitle = map[string]answer{}
	}
	a, ok := c.visible.subtitle[subID]
	c.visible.mu.Unlock()
	if ok && now.Before(a.until) {
		return a.item, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/api/v1/subtitles/"+url.PathEscape(subID)+"/asset", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("katalog subtitle: %w", err)
	}
	defer resp.Body.Close()
	item := ""
	switch resp.StatusCode {
	case http.StatusOK:
		var raw struct {
			ItemID string `json:"itemId"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return "", fmt.Errorf("katalog subtitle: %w", err)
		}
		item = raw.ItemID
	case http.StatusNotFound:
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("katalog subtitle: %d %s", resp.StatusCode, string(body))
	}
	c.visible.mu.Lock()
	keep(c.visible.subtitle, subID, answer{item: item, until: now.Add(subtitleTTL)}, now)
	c.visible.mu.Unlock()
	return item, nil
}
