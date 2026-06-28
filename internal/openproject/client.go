// Package openproject is a minimal client over the OpenProject REST v3
// API — just enough surface for the feedback
// endpoint: create a bug work package, attach a screenshot, append a
// comment. Auth is HTTP Basic with the literal user "apikey" and a
// service-user PAT as the password (OpenProject's API-key convention).
package openproject

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned by AddComment when the target work package no
// longer exists (deleted in the OpenProject UI). The feedback handler
// uses it to fall back to opening a fresh ticket instead of failing the
// report.
var ErrNotFound = errors.New("openproject: work package not found")

type Client struct {
	// BaseURL is the OpenProject origin, e.g. https://openproject.example
	// (no trailing slash; New normalizes).
	BaseURL string
	// Token is the service-user personal access token. Sent as HTTP
	// Basic "apikey:<token>".
	Token string
	// ProjectID + BugTypeID select where new work packages land — the
	// feedback project and its Bug type.
	ProjectID int
	BugTypeID int

	HTTP *http.Client
}

// New wires the client. 15 s total timeout — these are small JSON
// bodies / screenshot uploads; anything slower means OpenProject is
// unhealthy and the report should fail fast rather than pile up
// goroutines behind a stuck upstream.
func New(baseURL, token string, projectID, bugTypeID int) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Token:     token,
		ProjectID: projectID,
		BugTypeID: bugTypeID,
		HTTP:      &http.Client{Timeout: 15 * time.Second},
	}
}

// WorkPackageURL is the human-facing link for a ticket, returned to the
// client so a manual reporter can follow their report.
func (c *Client) WorkPackageURL(id int) string {
	return c.BaseURL + "/work_packages/" + strconv.Itoa(id)
}

// CreateWorkPackage opens a new bug work package in the configured
// project and returns its id.
func (c *Client) CreateWorkPackage(ctx context.Context, subject, markdownDescription string) (int, error) {
	body, err := json.Marshal(map[string]any{
		"subject": subject,
		"description": map[string]string{
			"format": "markdown",
			"raw":    markdownDescription,
		},
		"_links": map[string]any{
			"project": map[string]string{"href": "/api/v3/projects/" + strconv.Itoa(c.ProjectID)},
			"type":    map[string]string{"href": "/api/v3/types/" + strconv.Itoa(c.BugTypeID)},
		},
	})
	if err != nil {
		return 0, fmt.Errorf("openproject: encode work package: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/v3/work_packages", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("openproject: create work package: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return 0, fmt.Errorf("openproject: create work package: %s", respError(resp))
	}
	var out struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("openproject: decode work package response: %w", err)
	}
	return out.ID, nil
}

// AddAttachment uploads a file onto an existing work package. The v3
// attachments endpoint wants multipart with a JSON "metadata" part
// (fileName) followed by the "file" part carrying the bytes.
func (c *Client) AddAttachment(ctx context.Context, wpID int, fileName, contentType string, data []byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	mh := textproto.MIMEHeader{}
	mh.Set("Content-Disposition", `form-data; name="metadata"`)
	mh.Set("Content-Type", "application/json")
	meta, err := mw.CreatePart(mh)
	if err != nil {
		return fmt.Errorf("openproject: build attachment metadata: %w", err)
	}
	metaJSON, err := json.Marshal(map[string]string{"fileName": fileName})
	if err != nil {
		return fmt.Errorf("openproject: encode attachment metadata: %w", err)
	}
	if _, err := meta.Write(metaJSON); err != nil {
		return fmt.Errorf("openproject: write attachment metadata: %w", err)
	}

	fh := textproto.MIMEHeader{}
	fh.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, fileName))
	fh.Set("Content-Type", contentType)
	file, err := mw.CreatePart(fh)
	if err != nil {
		return fmt.Errorf("openproject: build attachment file part: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("openproject: write attachment bytes: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("openproject: finalize attachment body: %w", err)
	}

	resp, err := c.do(ctx, http.MethodPost,
		"/api/v3/work_packages/"+strconv.Itoa(wpID)+"/attachments",
		mw.FormDataContentType(), &buf)
	if err != nil {
		return fmt.Errorf("openproject: add attachment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("openproject: add attachment: %s", respError(resp))
	}
	return nil
}

// AddComment appends a comment ("activity" in v3 terms) to an existing
// work package. A 404 surfaces as ErrNotFound so the caller can detect
// "ticket was deleted" and open a fresh one.
func (c *Client) AddComment(ctx context.Context, wpID int, raw string) error {
	body, err := json.Marshal(map[string]any{
		"comment": map[string]string{"raw": raw},
	})
	if err != nil {
		return fmt.Errorf("openproject: encode comment: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost,
		"/api/v3/work_packages/"+strconv.Itoa(wpID)+"/activities",
		"application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("openproject: add comment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("openproject: add comment to #%d: %w", wpID, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("openproject: add comment: %s", respError(resp))
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("apikey", c.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.HTTP.Do(req)
}

// respError condenses a non-2xx response into one error string —
// status line plus the first few hundred bytes of body (OpenProject
// returns a JSON error document with a useful "message").
func respError(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		return resp.Status
	}
	return resp.Status + ": " + msg
}
