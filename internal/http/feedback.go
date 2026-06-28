package http

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/openproject"
	"github.com/zaentrum/chino-api/internal/store"
)

// Limits for POST /api/v1/feedback. The screenshot dominates: 3 MB of
// image + 16 KB of report text fits comfortably under the 6 MB body
// cap even with multipart framing overhead.
const (
	maxFeedbackBody     = 6 << 20  // total multipart body
	maxScreenshotBytes  = 3 << 20  // per the shared API contract
	maxDescriptionBytes = 16 << 10 // crash stacks are truncated, not rejected
	maxSubjectRunes     = 120
	feedbackDedupWindow = 14 * 24 * time.Hour
	feedbackRateLimit   = 5                // reports …
	feedbackRateWindow  = 10 * time.Minute // … per user per window
)

// feedbackReport is the decoded "report" multipart part — the contract
// shared by chino-web, chino-mobile and chino-androidtv. `context` is a
// flat string map (app version, route, item id, user agent, …) so the
// server can render it as a table without knowing the keys.
type feedbackReport struct {
	Source      string            `json:"source"` // web | mobile | tv
	Kind        string            `json:"kind"`   // manual | error | crash | player
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Fingerprint string            `json:"fingerprint"` // sha-256 hex of the normalized error signature; empty for manual
	Context     map[string]string `json:"context"`
}

// feedbackLimiter is a per-user sliding-window rate limiter. In-memory
// is fine here: chino-api runs single-replica (k8s/deployment.yaml) and
// losing the window on a restart only hands users a fresh budget.
type feedbackLimiter struct {
	mu   sync.Mutex
	seen map[string][]time.Time
}

// allow records the attempt and reports whether the user is still under
// the budget. Expired timestamps are pruned on every call so the map
// stays proportional to recently-active reporters.
func (l *feedbackLimiter) allow(userID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-feedbackRateWindow)
	kept := l.seen[userID][:0]
	for _, t := range l.seen[userID] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= feedbackRateLimit {
		l.seen[userID] = kept
		return false
	}
	l.seen[userID] = append(kept, now)
	return true
}

// postFeedback turns a client bug report into an OpenProject Bug work
// package. Auto reports (kind error/crash/player) carry a fingerprint
// and are deduplicated: a recurrence within feedbackDedupWindow only
// appends a comment to the existing ticket. Clients silently drop auto
// reports on any non-2xx, so error responses here never reach a user —
// only the manual-report dialog surfaces them.
func postFeedback(st *store.Store, op *openproject.Client) http.HandlerFunc {
	limiter := &feedbackLimiter{seen: map[string][]time.Time{}}
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		if op == nil {
			// OPENPROJECT_TOKEN unset — feature off (local dev,
			// self-host installs). Clients treat 503 as "don't report".
			http.Error(w, "feedback not configured", http.StatusServiceUnavailable)
			return
		}
		if !limiter.allow(userID) {
			http.Error(w, "too many reports", http.StatusTooManyRequests)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxFeedbackBody)
		if err := r.ParseMultipartForm(maxFeedbackBody); err != nil {
			status := http.StatusBadRequest
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "bad multipart: "+err.Error(), status)
			return
		}
		defer func() { _ = r.MultipartForm.RemoveAll() }()

		rep, err := decodeFeedbackReport(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		screenshot, screenshotCT, err := readScreenshot(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		username := reporterName(r, userID)

		// Dedup path — only when the client computed a fingerprint and
		// the report isn't hand-written. A nil store (no PG_URL) skips
		// dedup entirely and every report opens a fresh ticket.
		if rep.Fingerprint != "" && rep.Kind != "manual" {
			row, err := st.GetFeedbackReport(ctx, rep.Fingerprint)
			if err != nil {
				http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if row != nil && time.Since(row.LastSeen) <= feedbackDedupWindow {
				count, err := st.BumpFeedbackReport(ctx, rep.Fingerprint)
				if err != nil {
					http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
					return
				}
				err = op.AddComment(ctx, row.WorkPackageID, recurredComment(username, rep, count))
				switch {
				case err == nil:
					writeJSON(w, http.StatusOK, map[string]any{
						"id":        row.WorkPackageID,
						"url":       op.WorkPackageURL(row.WorkPackageID),
						"duplicate": true,
					})
					return
				case errors.Is(err, openproject.ErrNotFound):
					// Ticket was deleted in OpenProject — fall through to
					// open a fresh one; UpsertFeedbackReport below
					// re-points the row and resets the count.
				default:
					http.Error(w, "openproject: "+err.Error(), http.StatusBadGateway)
					return
				}
			}
			// Row absent, aged out of the window, or its ticket deleted:
			// create a new work package below and (re-)point the row.
		}

		wpID, err := op.CreateWorkPackage(ctx, feedbackSubject(rep), feedbackDescription(username, rep, screenshot != nil))
		if err != nil {
			http.Error(w, "openproject: "+err.Error(), http.StatusBadGateway)
			return
		}
		if screenshot != nil {
			ext := "jpg"
			if screenshotCT == "image/png" {
				ext = "png"
			}
			name := fmt.Sprintf("screenshot-%d.%s", wpID, ext)
			if err := op.AddAttachment(ctx, wpID, name, screenshotCT, screenshot); err != nil {
				// Non-fatal: the ticket exists, just without the image.
				slog.Warn("feedback: attach screenshot failed", "wp", wpID, "err", err)
			}
		}
		if rep.Fingerprint != "" && rep.Kind != "manual" {
			if err := st.UpsertFeedbackReport(ctx, rep.Fingerprint, wpID); err != nil {
				// Non-fatal: worst case the next recurrence opens a dup.
				slog.Warn("feedback: record fingerprint failed", "fingerprint", rep.Fingerprint, "err", err)
			}
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"id":        wpID,
			"url":       op.WorkPackageURL(wpID),
			"duplicate": false,
		})
	}
}

// decodeFeedbackReport pulls the "report" JSON part out of the parsed
// multipart form and validates it against the shared contract. The part
// usually arrives as a plain form value; some multipart writers attach
// a filename to the JSON part, which lands it under File instead — we
// accept both.
func decodeFeedbackReport(r *http.Request) (*feedbackReport, error) {
	raw := r.FormValue("report")
	if raw == "" && r.MultipartForm != nil {
		if fhs := r.MultipartForm.File["report"]; len(fhs) > 0 {
			f, err := fhs[0].Open()
			if err != nil {
				return nil, fmt.Errorf("open report part: %w", err)
			}
			defer f.Close()
			b, err := io.ReadAll(io.LimitReader(f, maxDescriptionBytes+(64<<10)))
			if err != nil {
				return nil, fmt.Errorf("read report part: %w", err)
			}
			raw = string(b)
		}
	}
	if raw == "" {
		return nil, errors.New("missing report part")
	}
	var rep feedbackReport
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		return nil, fmt.Errorf("bad report JSON: %w", err)
	}
	switch rep.Source {
	case "web", "mobile", "tv":
	default:
		return nil, errors.New("invalid source")
	}
	switch rep.Kind {
	case "manual", "error", "crash", "player":
	default:
		return nil, errors.New("invalid kind")
	}
	rep.Description = strings.TrimSpace(rep.Description)
	if rep.Description == "" {
		return nil, errors.New("empty description")
	}
	if len(rep.Description) > maxDescriptionBytes {
		rep.Description = truncateUTF8(rep.Description, maxDescriptionBytes)
	}
	rep.Fingerprint = strings.ToLower(strings.TrimSpace(rep.Fingerprint))
	return &rep, nil
}

// readScreenshot returns the optional screenshot bytes + content type,
// or (nil, "", nil) when none was sent. Enforces the contract's type
// whitelist (png/jpeg) and 3 MB cap.
func readScreenshot(r *http.Request) ([]byte, string, error) {
	if r.MultipartForm == nil {
		return nil, "", nil
	}
	fhs := r.MultipartForm.File["screenshot"]
	if len(fhs) == 0 {
		return nil, "", nil
	}
	fh := fhs[0]
	ct := fh.Header.Get("Content-Type")
	if ct != "image/png" && ct != "image/jpeg" {
		return nil, "", errors.New("screenshot must be image/png or image/jpeg")
	}
	if fh.Size > maxScreenshotBytes {
		return nil, "", errors.New("screenshot too large (max 3 MB)")
	}
	f, err := fh.Open()
	if err != nil {
		return nil, "", fmt.Errorf("open screenshot: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxScreenshotBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read screenshot: %w", err)
	}
	if len(data) > maxScreenshotBytes {
		return nil, "", errors.New("screenshot too large (max 3 MB)")
	}
	return data, ct, nil
}

// feedbackSubject builds "[source][kind] <title or first description
// line>", truncated to maxSubjectRunes so OpenProject's subject column
// stays scannable.
func feedbackSubject(rep *feedbackReport) string {
	head := strings.TrimSpace(rep.Title)
	if head == "" {
		head = strings.TrimSpace(strings.SplitN(rep.Description, "\n", 2)[0])
	}
	s := "[" + rep.Source + "][" + rep.Kind + "] " + head
	if utf8.RuneCountInString(s) > maxSubjectRunes {
		runes := []rune(s)
		s = string(runes[:maxSubjectRunes-1]) + "…"
	}
	return s
}

// feedbackDescription renders the work-package body: reporter line,
// sorted context table, then the description — fenced for machine
// reports (stack traces keep their formatting) and plain prose for
// manual ones.
func feedbackDescription(username string, rep *feedbackReport, hasScreenshot bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Reported by:** %s via %s (%s)\n\n", username, rep.Source, rep.Kind)
	if len(rep.Context) > 0 {
		keys := make([]string, 0, len(rep.Context))
		for k := range rep.Context {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("| Context | Value |\n| --- | --- |\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "| %s | %s |\n", mdCell(k), mdCell(rep.Context[k]))
		}
		b.WriteString("\n")
	}
	if rep.Kind == "manual" {
		b.WriteString(rep.Description)
		b.WriteString("\n")
	} else {
		b.WriteString("```\n")
		b.WriteString(rep.Description)
		b.WriteString("\n```\n")
	}
	if hasScreenshot {
		b.WriteString("\nScreenshot attached.\n")
	}
	return b.String()
}

// recurredComment is the note appended to an existing ticket when its
// fingerprint recurs inside the dedup window.
func recurredComment(username string, rep *feedbackReport, count int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Recurred** — reported by %s via %s (%s)", username, rep.Source, rep.Kind)
	if v := appVersionFrom(rep.Context); v != "" {
		fmt.Fprintf(&b, ", app version %s", v)
	}
	fmt.Fprintf(&b, ". Seen %d times total.", count)
	return b.String()
}

// appVersionFrom digs the app version out of the free-form context map.
// The clients aren't pinned to one key name, so try the likely ones.
func appVersionFrom(ctx map[string]string) string {
	for _, k := range []string{"appVersion", "app_version", "version"} {
		if v := ctx[k]; v != "" {
			return v
		}
	}
	return ""
}

// reporterName extracts preferred_username from the already-verified
// bearer so tickets read with a human name instead of a Keycloak sub
// UUID. This is a payload decode, NOT a verification — the auth
// middleware verified the token before the handler ran. Falls back to
// the subject when the claim is missing (e.g. stream-token auth paths
// or client-credential tokens).
func reporterName(r *http.Request, sub string) string {
	raw := bearerFrom(r)
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return sub
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return sub
	}
	var claims struct {
		PreferredUsername string `json:"preferred_username"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.PreferredUsername == "" {
		return sub
	}
	return claims.PreferredUsername
}

// mdCell makes a context key/value safe inside a markdown table cell —
// pipes would split the cell, newlines would break the row.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
