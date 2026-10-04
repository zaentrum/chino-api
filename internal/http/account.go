package http

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/portal"
	"github.com/zaentrum/chino-api/internal/store"
)

// Deleting an account. A person deletes their own from any app — the app
// stores require it of every app that makes accounts — with DELETE /api/v1/me:
// chino-api deletes what it keeps of them (their progress, watch history,
// watchlists and likes) and asks portal-api, with the account deletion token
// and the person's own bearer, to delete their account in the platform's
// realm. The data is deleted inside a transaction that is committed only
// once portal-api has deleted the account — or found it gone already — and
// rolled back when portal-api refuses (the last admin; an administrator of
// Keycloak itself) or does not answer: nothing is deleted then, and asking
// again starts over.
//
// portal-api, deleting a person an admin deletes on the People page, asks
// for their data first with DELETE /api/v1/admin/accounts/{sub}/data — the
// account's subject, its Keycloak id — with the admin's bearer, holding the
// admin role, and the same token.

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
			slog.Error("account deletion: the data could not be deleted", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "data_not_deleted",
				"message": "Your data could not be deleted, so nothing was. Try again later."})
			return
		}
		err = pc.DeleteAccount(r.Context(), bearerFrom(r), token)
		var refused *portal.AccountRefused
		switch {
		case err == nil || errors.Is(err, portal.ErrAccountGone):
			if cerr := d.Commit(r.Context()); cerr != nil {
				slog.Error("account deletion: the account is gone, its data could not be deleted", "err", cerr)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "data_not_deleted",
					"message": "Your account is deleted; some of your data could not be. Sign in again and ask once more to remove it."})
				return
			}
			account := "deleted"
			if err != nil {
				account = "gone"
			}
			slog.Info("account deletion: a person deleted their account", "account", account,
				"progress", d.Deleted.Progress, "watched", d.Deleted.Watched, "watchlists", d.Deleted.Watchlists, "likes", d.Deleted.Likes)
			writeJSON(w, http.StatusOK, map[string]any{"account": account, "deleted": d.Deleted})
		case errors.As(err, &refused):
			_ = d.Rollback(r.Context())
			writeJSON(w, http.StatusConflict, map[string]any{"error": "refused", "message": refused.Message})
		default:
			_ = d.Rollback(r.Context())
			slog.Error("account deletion: portal-api did not delete the account", "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "account_not_deleted",
				"message": "Your account could not be deleted right now, so nothing was. Try again later."})
		}
	}
}

// deleteAccountData handles DELETE /api/v1/admin/accounts/{sub}/data: what
// chino-api keeps of an account an admin is deleting. For portal-api: the
// admin's bearer, with the admin role, and the account deletion token.
func deleteAccountData(st *store.Store, admin adminAccess, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !admin.allow(w, r) {
			return
		}
		if token == "" {
			writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "account_deletion_unavailable"})
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(portal.DeletionHeader)), []byte(token)) != 1 {
			slog.Warn("account deletion: an account's data was asked for without the deletion token")
			http.Error(w, "forbidden: an account's data is deleted with the account, by portal-api", http.StatusForbidden)
			return
		}
		d, err := st.DeleteUserData(r.Context(), chi.URLParam(r, "sub"))
		if err == nil {
			err = d.Commit(r.Context())
		}
		if err != nil {
			slog.Error("account deletion: an account's data could not be deleted", "err", err)
			http.Error(w, "the data could not be deleted", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": d.Deleted})
	}
}
