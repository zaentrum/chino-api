// Package portal is chino-api's client for portal-api: a thin read client for
// its UI extension registry — chino forwards the user's bearer to GET
// /api/portal/slots/{slot} so addon-contributed buttons can be surfaced
// natively in the SPA, best-effort: an unset base URL or an unreachable portal
// yields an empty slice, so an instance with no addon shows no extension UI
// (and chino never hard-depends on the portal being up) — the notices addons
// leave the signed-in person (/api/portal/me/notices, with their bearer, as
// best effort as the slots), and the one call account deletion makes, DELETE
// /api/portal/me.
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

// Notice mirrors portal-api's model.Notice: what an addon told the
// signed-in person. Its text is the addon's plain text; link and itemId are
// "" for none, readAt null while unread. Times pass through as portal-api
// writes them (RFC 3339).
type Notice struct {
	ID         string  `json:"id"`
	Addon      string  `json:"addon"`
	AddonTitle string  `json:"addonTitle"`
	AddonIcon  string  `json:"addonIcon"`
	Title      string  `json:"title"`
	Body       string  `json:"body"`
	Link       string  `json:"link"`
	ItemID     string  `json:"itemId"`
	CreatedAt  string  `json:"createdAt"`
	ReadAt     *string `json:"readAt"`
}

// NoticeList is the signed-in person's notices, newest first, and how many
// of theirs are unread.
type NoticeList struct {
	Notices []Notice `json:"notices"`
	Unread  int      `json:"unread"`
}

// Notices' answers to a change.
var (
	// ErrNoNotice: the person has no notice of that id — someone else's is
	// as one there is not.
	ErrNoNotice = errors.New("no such notice")
	// ErrNoticesUnavailable: portal-api did not answer, or not as it does.
	ErrNoticesUnavailable = errors.New("portal-api did not change the notice")
)

// Notices is the signed-in person's notices, read from portal-api with their
// bearer. ok is false — and the list empty — when there is no portal-api to
// ask, or it did not answer as it does: the caller shows none, and never
// fails for it.
func (c *Client) Notices(ctx context.Context, bearer string) (list NoticeList, ok bool) {
	empty := NoticeList{Notices: []Notice{}}
	if !c.Enabled() || bearer == "" {
		return empty, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/portal/me/notices", nil)
	if err != nil {
		return empty, false
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return empty, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return empty, false
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&list); err != nil {
		return empty, false
	}
	if list.Notices == nil {
		list.Notices = []Notice{}
	}
	return list, true
}

// ReadNotice marks one of the person's notices read, and answers how many of
// theirs are unread still.
func (c *Client) ReadNotice(ctx context.Context, bearer, id string) (int, error) {
	var out struct {
		Unread int `json:"unread"`
	}
	err := c.changeNotice(ctx, bearer, http.MethodPost, "/api/portal/me/notices/"+url.PathEscape(id)+"/read", &out)
	return out.Unread, err
}

// ReadAllNotices marks every notice of the person read, and answers how
// many it marked.
func (c *Client) ReadAllNotices(ctx context.Context, bearer string) (int, error) {
	var out struct {
		Read int `json:"read"`
	}
	err := c.changeNotice(ctx, bearer, http.MethodPost, "/api/portal/me/notices/read-all", &out)
	return out.Read, err
}

// DeleteNotice deletes one of the person's notices.
func (c *Client) DeleteNotice(ctx context.Context, bearer, id string) error {
	return c.changeNotice(ctx, bearer, http.MethodDelete, "/api/portal/me/notices/"+url.PathEscape(id), nil)
}

// changeNotice sends a change of the person's notices with their bearer and
// reads what portal-api answers into out: ErrNoNotice for a notice they do
// not have, ErrNoticesUnavailable for anything but an answer.
func (c *Client) changeNotice(ctx context.Context, bearer, method, path string, out any) error {
	if !c.Enabled() || bearer == "" {
		return ErrNoticesUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoticesUnavailable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNoNotice
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("%w: it answered %d", ErrNoticesUnavailable, resp.StatusCode)
	case out == nil:
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out); err != nil {
		return fmt.Errorf("%w: %v", ErrNoticesUnavailable, err)
	}
	return nil
}
