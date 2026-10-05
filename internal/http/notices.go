package http

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/portal"
)

// Notices: what addons tell the signed-in person — "your title is ready" —
// kept by portal-api, which shows each person their own and nobody else's.
// The apps read and change them here; chino-api forwards the viewer's bearer
// to portal-api's /api/portal/me/notices and keeps nothing of them.
//
//	GET    /api/v1/notices                  their notices, newest first, how many are unread, and whether portal-api answered
//	POST   /api/v1/notices/{noticeId}/read  one of theirs, read
//	POST   /api/v1/notices/read-all         all of theirs, read
//	DELETE /api/v1/notices/{noticeId}       one of theirs, deleted
//
// The parameter is noticeId, never id: on chino-api's routes {id} is a
// title's, which the parental gate holds to a capped viewer's cap.
//
// Best effort, as the slots are: an unset or unreachable portal-api answers
// the list empty with available false, so a home screen that shows notices
// never fails for them; a change portal-api cannot make answers 503 (none
// configured) or 502 (it did not answer), and the app keeps the notice as it
// was. What a notice says is the addon's plain text: an app shows it as text,
// follows its link only to its own server, and opens its itemId through the
// item routes, which hold a capped viewer to their cap.

// noticeID is what a notice's id may be on the way to portal-api: letters,
// digits and dashes — portal-api's ids are UUIDs — so nothing that reads as
// more than one path segment is forwarded.
var noticeID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// listNotices handles GET /api/v1/notices: always 200.
func listNotices(pc *portal.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, ok := pc.Notices(r.Context(), bearerFrom(r))
		writeJSON(w, http.StatusOK, map[string]any{"notices": list.Notices, "unread": list.Unread, "available": ok})
	}
}

// readNotice handles POST /api/v1/notices/{noticeId}/read: {unread} still.
func readNotice(pc *portal.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "noticeId")
		if !noticeID.MatchString(id) {
			noNotice(w)
			return
		}
		unread, err := pc.ReadNotice(r.Context(), bearerFrom(r), id)
		if noticeChanged(w, pc, err) {
			writeJSON(w, http.StatusOK, map[string]any{"unread": unread})
		}
	}
}

// readAllNotices handles POST /api/v1/notices/read-all: {read, unread}.
func readAllNotices(pc *portal.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		read, err := pc.ReadAllNotices(r.Context(), bearerFrom(r))
		if noticeChanged(w, pc, err) {
			writeJSON(w, http.StatusOK, map[string]any{"read": read, "unread": 0})
		}
	}
}

// deleteNotice handles DELETE /api/v1/notices/{noticeId}: 204.
func deleteNotice(pc *portal.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "noticeId")
		if !noticeID.MatchString(id) {
			noNotice(w)
			return
		}
		if noticeChanged(w, pc, pc.DeleteNotice(r.Context(), bearerFrom(r), id)) {
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func noNotice(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found", "message": "No such notice."})
}

// noticeChanged answers a change portal-api did not make; true when it made
// it, and the caller answers.
func noticeChanged(w http.ResponseWriter, pc *portal.Client, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, portal.ErrNoNotice):
		noNotice(w)
	case !pc.Enabled():
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "notices_unavailable",
			"message": "This server keeps no notices."})
	default:
		slog.Warn("notices: portal-api did not change a notice", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "notices_unavailable",
			"message": "Notices cannot be changed right now. Try again later."})
	}
	return false
}
