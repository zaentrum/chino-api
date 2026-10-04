package http

import (
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/redact"
)

// adminAccess is who may call the /api/v1/admin/* routes: a caller whose
// verified bearer carries the admin role in its realm roles
// (realm_access.roles), as katalog-manager and the portal tell an
// administrator; and, deprecated, a caller whose subject ADMIN_SUBJECTS
// lists, let through besides it.
type adminAccess struct {
	role     string
	subjects map[string]bool
}

// newAdminAccess is the admin access for role (config.DefaultAdminRole when
// empty) and the deprecated subjects.
func newAdminAccess(role string, subjects []string) adminAccess {
	if role == "" {
		role = config.DefaultAdminRole
	}
	a := adminAccess{role: role, subjects: map[string]bool{}}
	for _, s := range subjects {
		a.subjects[s] = true
	}
	return a
}

// allow reports whether the request's caller is an admin, and otherwise
// answers it: 401 without a subject, 403 naming the role for anyone else.
func (a adminAccess) allow(w http.ResponseWriter, r *http.Request) bool {
	sub, err := auth.SubjectFromContext(r.Context())
	if err != nil || sub == "" {
		http.Error(w, "no subject", http.StatusUnauthorized)
		return false
	}
	if auth.HasRole(r.Context(), a.role) || a.subjects[sub] {
		return true
	}
	http.Error(w, "admin access required: the "+a.role+" role", http.StatusForbidden)
	return false
}

// postPackageRequest forwards POST /api/v1/admin/items/{id}/package to
// katalog-manager's POST /api/items/{id}/package, an admin's packaging action:
// what its GraphQL packageItem does, the item's packaging enqueued. It answers
// {status, alreadyActive, message} for a movie or an episode and
// {episodesEnqueued, episodesTotal, message} for a series, 404 for an unknown
// item and 400 for one that cannot be packaged; asking again for an item
// whose packaging is under way changes nothing and says so.
func postPackageRequest(admin adminAccess, managerBase string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !admin.allow(w, r) {
			return
		}
		id := chi.URLParam(r, "id")
		proxyToKatalogManager(w, r, managerBase, http.MethodPost, "/api/items/"+url.PathEscape(id)+"/package")
	}
}

// getPackageStatus forwards GET /api/v1/admin/items/{id}/package to
// katalog-manager's GET /api/analyze/items/{id}/steps: the item's processing
// steps, {"itemId": ..., "steps": {step: status}} (e.g. {"transcode":"done",
// "package":"in_progress"}), which a client polls to watch an item move
// through the pipeline.
func getPackageStatus(admin adminAccess, managerBase string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !admin.allow(w, r) {
			return
		}
		id := chi.URLParam(r, "id")
		proxyToKatalogManager(w, r, managerBase, http.MethodGet, "/api/analyze/items/"+url.PathEscape(id)+"/steps")
	}
}

// proxyToKatalogManager forwards an admin request to katalog-manager at
// base, the caller's bearer with it: katalog-manager is a resource server of
// the same realm and decides itself what the admin may do (its package
// action is an admin's, its steps an admin's or the service account's). Its
// answer comes back as it is, status, headers and body.
func proxyToKatalogManager(w http.ResponseWriter, r *http.Request, base, method, path string) {
	if base == "" {
		http.Error(w, "katalog-manager is not configured (KATALOG_MANAGER_URL)", http.StatusServiceUnavailable)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), method, strings.TrimRight(base, "/")+path, http.NoBody)
	if err != nil {
		http.Error(w, "bad katalog-manager url", http.StatusInternalServerError)
		return
	}
	// The bearer the middleware verified: the header, or the deprecated
	// ?token= it moved into the header.
	if bearer := r.Header.Get("Authorization"); strings.HasPrefix(bearer, "Bearer ") {
		req.Header.Set("Authorization", bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "katalog-manager upstream: "+redact.Text(err.Error()), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
