package katalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// Person is a cast/crew member. Credits is their catalogue appearance
// count (filled by SearchPeople so the UI can show "· 12 titles").
type Person struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Credits int    `json:"credits,omitempty"`
}

// PersonDetail is a person plus their filmography. Items are full chino
// Items (poster/backdrop URLs synthesised), so clients render them with
// the existing poster-card components.
type PersonDetail struct {
	Person
	Items []Item `json:"items"`
}

// SearchPeople proxies katalog-api's /people name search (accent- and
// case-insensitive). Empty q returns an empty slice without a round-trip.
func (c *Client) SearchPeople(ctx context.Context, bearer, q string, limit int) ([]Person, error) {
	if q == "" {
		return []Person{}, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	u := c.BaseURL + "/api/v1/people?q=" + url.QueryEscape(q) + "&limit=" + strconv.Itoa(limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("katalog people: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("katalog people: %d %s", resp.StatusCode, string(body))
	}
	var raw struct {
		People []Person `json:"people"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	if raw.People == nil {
		raw.People = []Person{}
	}
	return raw.People, nil
}

// GetPerson proxies katalog-api's /people/{id} — the person plus their
// filmography. Returns (nil, nil) on 404 so the handler answers 404. The
// filmography items are converted via toItem() so poster/backdrop URLs
// are present (watched_at is stamped by the chino-api handler).
func (c *Client) GetPerson(ctx context.Context, bearer, id string, limit int) (*PersonDetail, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	u := c.BaseURL + "/api/v1/people/" + url.PathEscape(id) + "?limit=" + strconv.Itoa(limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("katalog person: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("katalog person: %d %s", resp.StatusCode, string(body))
	}
	var raw struct {
		ID    string         `json:"id"`
		Name  string         `json:"name"`
		Items []upstreamItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	pd := &PersonDetail{
		Person: Person{ID: raw.ID, Name: raw.Name},
		Items:  make([]Item, 0, len(raw.Items)),
	}
	for _, it := range raw.Items {
		pd.Items = append(pd.Items, it.toItem())
	}
	return pd, nil
}
