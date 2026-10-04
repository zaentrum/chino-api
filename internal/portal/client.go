// Package portal is chino-api's client for portal-api: a thin read client for
// its UI extension registry — chino forwards the user's bearer to GET
// /api/portal/slots/{slot} so addon-contributed buttons can be surfaced
// natively in the SPA, best-effort: an unset base URL or an unreachable portal
// yields an empty slice, so an instance with no addon shows no extension UI
// (and chino never hard-depends on the portal being up) — and the one call
// account deletion makes, DELETE /api/portal/me.
package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// Deletion is the client account deletion calls with: portal-api reads
	// the realm and deletes the account in Keycloak, which takes longer than
	// a slot read may.
	Deletion *http.Client
}

// DeletionHeader carries the account deletion token between chino-api and
// portal-api, both ways: what says a call comes from the service that deletes
// a person's data, and from no client.
const DeletionHeader = "X-Account-Deletion-Token"

// Account deletion's answers.
var (
	// ErrAccountGone: portal-api has no such account (deleted already).
	ErrAccountGone = errors.New("the account is gone already")
	// ErrPortalUnavailable: portal-api did not answer, or not as it does.
	ErrPortalUnavailable = errors.New("portal-api did not delete the account")
)

// AccountRefused is portal-api refusing to delete the account: the last
// admin, an administrator of Keycloak itself. Message is its words for the
// person.
type AccountRefused struct{ Message string }

func (e *AccountRefused) Error() string { return e.Message }

// DeleteAccount asks portal-api to delete the account of the person whose
// bearer this is (DELETE /api/portal/me), showing the account deletion
// token: nil when it is deleted, ErrAccountGone when there was none,
// *AccountRefused when portal-api will not, ErrPortalUnavailable otherwise.
func (c *Client) DeleteAccount(ctx context.Context, bearer, token string) error {
	if !c.Enabled() || bearer == "" || token == "" {
		return ErrPortalUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+"/api/portal/me", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set(DeletionHeader, token)
	resp, err := c.Deletion.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPortalUnavailable, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusNotFound:
		return ErrAccountGone
	case http.StatusConflict:
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = "This account cannot be deleted from here."
		}
		return &AccountRefused{Message: msg}
	}
	return fmt.Errorf("%w: it answered %d", ErrPortalUnavailable, resp.StatusCode)
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		HTTP:     &http.Client{Timeout: 5 * time.Second},
		Deletion: &http.Client{Timeout: 30 * time.Second},
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
