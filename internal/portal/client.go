// Package portal is a thin read client for portal-api's UI extension registry.
// chino forwards the user's bearer to GET /api/portal/slots/{slot} so addon-
// contributed buttons can be surfaced natively in the SPA. It is best-effort:
// an unset base URL or an unreachable portal yields an empty slice, so an
// instance with no addon shows no extension UI (and chino never hard-depends on
// the portal being up).
package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Extension mirrors the portal-api model — the fields the SPA renders.
type Extension struct {
	Key       string `json:"key"`
	Addon     string `json:"addon"`
	Slot      string `json:"slot"`
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Icon      string `json:"icon"`
	URL       string `json:"url"`
	Method    string `json:"method"`
	StatusURL string `json:"statusUrl"`
	Order     int    `json:"ord"`
	Enabled   bool   `json:"enabled"`
}

// Client talks to portal-api. A blank BaseURL disables it.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

// Enabled reports whether a portal base URL is configured.
func (c *Client) Enabled() bool { return c != nil && c.BaseURL != "" }

// SlotExtensions returns the enabled contributions for a slot, forwarding the
// user's bearer. Any error (disabled, unreachable, non-200, bad body) returns
// an empty slice with a nil error — the slot simply renders nothing.
func (c *Client) SlotExtensions(ctx context.Context, slot, bearer string) []Extension {
	if !c.Enabled() || slot == "" {
		return nil
	}
	u := c.BaseURL + "/api/portal/slots/" + url.PathEscape(slot)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out []Extension
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil
	}
	return out
}
