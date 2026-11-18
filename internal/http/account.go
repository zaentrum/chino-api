package http

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/logline"
	"github.com/zaentrum/chino-api/internal/portal"
	"github.com/zaentrum/chino-api/internal/store"
)

// Deleting an account. A person deletes their own from any app — the app
// stores require it of every app that makes accounts — with DELETE /api/v1/me:
// chino-api deletes what it keeps of them (their progress, watch history,
// watchlists, likes and the settings their account keeps) and asks portal-api, with the account deletion token
// and the person's own bearer, to delete their account in the platform's
// realm. The data is deleted inside a transaction that is committed only
// once portal-api has deleted the account — or found it gone already — and
// rolled back when portal-api refuses (the last admin; an administrator of
// Keycloak itself) or does not answer: nothing is deleted then, and asking
// again starts over.
//
// Once the deletion has begun, nothing more is kept for the person: a write
// sent while it is open waits for its end, and once it is committed that
// write, and any after it with a bearer still valid, is refused with 401
// account_deleted (storeFailed), on which the app signs the account out. The
// account's id is kept for store.DeletedAccountKept to refuse them, and
// nothing else of it; a deletion rolled back keeps no id, and the waiting
// write is written.
//
// portal-api, deleting a person an admin deletes on the People page, asks
// for their data first with DELETE /api/v1/admin/accounts/{sub}/data — the
// account's subject, its Keycloak id — with the admin's bearer, holding the
// admin role, and the same token. That deletion is committed before
// portal-api deletes the account itself; where that later step fails, the
// person still has their account, and portal-api asks chino-api to forget the
// deletion with DELETE /api/v1/admin/accounts/{sub}/deleted?deletion=, the
// value the data's answer gave, so that their writes go in again at once.

// deletionFinish bounds what a person's deletion does once it has begun, on
// a context of its own: the calls by which portal-api deletes the account,
// and the commit or rollback after them. deletionAsk bounds each call, 20 s,
// so that where the first gives no answer a second fits; deletionEnd, 15 s,
// is kept for the commit or rollback: two asks and the end add up to
// deletionFinish, 55 s. deletionAsk is a variable for the tests alone.
const (
	deletionEnd    = 15 * time.Second
	deletionFinish = 2*20*time.Second + deletionEnd
)

var deletionAsk = 20 * time.Second

// askPortal asks portal-api to delete the account of r's bearer, bounded by
// deletionAsk; where it gives no answer, or a 5xx, it asks once more: an
// account portal-api deleted, whose answer did not come, is answered gone
// the second time. A 4xx is not asked again: it would be answered the same.
func askPortal(ctx context.Context, r *http.Request, pc *portal.Client, token string) error {
	ask := func() error {
		ctx, cancel := context.WithTimeout(ctx, deletionAsk)
		defer cancel()
		return pc.DeleteAccount(ctx, bearerFrom(r), token)
	}
	err := ask()
	var answered *portal.Answered
	if errors.Is(err, portal.ErrPortalUnavailable) && (!errors.As(err, &answered) || answered.Status >= 500) {
		logErrorAt(slog.LevelWarn, r, "account deletion: portal-api gave no answer, or a 5xx; asked once more", err)
		err = ask()
	}
	return err
}

// deleteMe handles DELETE /api/v1/me.
func deleteMe(st *store.Store, pc *portal.Client, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("token") {
			// The deprecated bearer in the query string: a link someone
			// could be sent. An account is deleted with the header only.
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bearer_in_url",
				"message": "Send the bearer in the Authorization header to delete an account."})
			return
		}
		if token == "" || !pc.Enabled() {
			writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "account_deletion_unavailable",
				"message": "Accounts are not deleted from the apps on this server: ask whoever runs it."})
			return
		}
		sub, err := auth.SubjectFromContext(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		d, err := st.DeleteUserData(r.Context(), sub)
		if err != nil {
			logFailureAt(slog.LevelError, r, "the database", "the account's deletion", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "data_not_deleted",
				"message": "Your data could not be deleted, so nothing was. Try again later."})
			return
		}
		// Begun, the deletion goes to its end whether or not the caller
		// waits for the answer: once portal-api has deleted the account, the
		// commit must follow. Its own context, which the caller's leaving
		// does not cancel, bounded by deletionFinish: the calls to
		// portal-api, then deletionEnd for the commit or rollback.
		finish := context.WithoutCancel(r.Context())
		asks, cancelAsks := context.WithTimeout(finish, deletionFinish-deletionEnd)
		defer cancelAsks()
		err = askPortal(asks, r, pc, token)
		ctx, cancel := context.WithTimeout(finish, deletionEnd)
		defer cancel()
		var refused *portal.AccountRefused
		switch {
		case err == nil || errors.Is(err, portal.ErrAccountGone):
			if cerr := d.Commit(ctx); cerr != nil {
				logErrorAt(slog.LevelError, r, "account deletion: the account is gone, its data could not be deleted", cerr)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "data_not_deleted",
					"message": "Your account is deleted; some of your data could not be. Sign in again and ask once more to remove it."})
				return
			}
			account := "deleted"
			if err != nil {
				account = "gone"
			}
			slog.Info("account deletion: a person deleted their account", "account", account,
				"progress", d.Deleted.Progress, "watched", d.Deleted.Watched, "watchlists", d.Deleted.Watchlists, "likes", d.Deleted.Likes, "settings", d.Deleted.Settings)
			writeJSON(w, http.StatusOK, map[string]any{"account": account, "deleted": d.Deleted})
		case errors.As(err, &refused):
			_ = d.Rollback(ctx)
			writeJSON(w, http.StatusConflict, map[string]any{"error": "refused", "message": refused.Message})
		default:
			_ = d.Rollback(ctx)
			logErrorAt(slog.LevelError, r, "account deletion: portal-api did not delete the account", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "account_not_deleted",
				"message": "Your account could not be deleted right now, so nothing was. Try again later."})
		}
	}
}

// fromPortal reports whether r is portal-api's, on a route of an admin's
// deletion of an account: an admin's bearer, with the admin role, and the
// account deletion token. Else it has answered r: 501 where no token is set
// up, 403 without it.
func fromPortal(w http.ResponseWriter, r *http.Request, admin adminAccess, token string) bool {
	if !admin.allow(w, r) {
		return false
	}
	if token == "" {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "account_deletion_unavailable"})
		return false
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(portal.DeletionHeader)), []byte(token)) != 1 {
		slog.Warn("account deletion: an admin's route was asked for without the deletion token", "route", logline.Route(r))
		http.Error(w, "forbidden: an account's data is deleted with the account, by portal-api", http.StatusForbidden)
		return false
	}
	return true
}

// deleteAccountData handles DELETE /api/v1/admin/accounts/{sub}/data: what
// chino-api keeps of an account an admin is deleting. For portal-api: the
// admin's bearer, with the admin role, and the account deletion token.
func deleteAccountData(st *store.Store, admin adminAccess, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !fromPortal(w, r, admin, token) {
			return
		}
		d, err := st.DeleteUserData(r.Context(), chi.URLParam(r, "sub"))
		if err == nil {
			err = d.Commit(r.Context())
		}
		if err != nil {
			logFailureAt(slog.LevelError, r, "the database", "an account's data, for portal-api", err)
			http.Error(w, "the data could not be deleted", http.StatusInternalServerError)
			return
		}
		answer := map[string]any{"deleted": d.Deleted}
		if d.ID != "" {
			// What the deletion is forgotten by, should the account stay.
			answer["deletion"] = d.ID
		}
		writeJSON(w, http.StatusOK, answer)
	}
}

// forgetDeletion handles DELETE
// /api/v1/admin/accounts/{sub}/deleted?deletion=…: an admin's deletion of an
// account whose later step failed. portal-api deletes the account's data
// first (deleteAccountData, which answers the deletion's value) and the
// account after; where that fails, the person still has their account, and
// chino-api stops keeping that deletion's id (store.ForgetDeletion): their
// writes go in again at once. The data deleted stays deleted. Where the id
// kept is another deletion's (the person's own, since) or none is, there is
// nothing of this deletion's to undo: 409 deletionNotKept, and the id stays.
// For portal-api, as the data: an admin's bearer, with the admin role, and
// the account deletion token. 204; 400 without deletion, or with it twice.
func forgetDeletion(st *store.Store, admin adminAccess, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !fromPortal(w, r, admin, token) || !once(w, r, "deletion", "deletion is given once") {
			return
		}
		deletion := r.URL.Query().Get("deletion")
		if deletion == "" {
			http.Error(w, "deletion is required", http.StatusBadRequest)
			return
		}
		err := st.ForgetDeletion(r.Context(), chi.URLParam(r, "sub"), deletion)
		switch {
		case errors.Is(err, store.ErrDeletionNotKept):
			writeJSON(w, http.StatusConflict, deletionNotKept)
		case accountRefused(w, r, err, "an undo of a deletion"):
		case err != nil:
			storeFailed(w, r, err)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// deletionNotKept is the answer, with 409, to the undoing of a deletion of
// which no id is kept: it was undone already or is past its hour, or another
// deletion of the account has kept its own since, which stays. A code of
// ADR-0023, part 4, that needs no values.
var deletionNotKept = codeAnswer{Error: "deletion_not_kept",
	Detail: "no id of this deletion is kept: nothing of its own to undo"}
