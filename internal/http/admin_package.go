package http

import (
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/auth"
)

// admin endpoints share a single allowlist check. Kept in a package
// variable so the constructor (router.go) can populate it once from
// cfg.AdminSubjects; the handler closures below close over it via
// requireAdmin.
var adminSubjects = map[string]struct{}{}

// SetAdminSubjects populates the in-package allowlist. Called once
// from router construction.
func SetAdminSubjects(subs []string) {
	adminSubjects = make(map[string]struct{}, len(subs))
	for _, s := range subs {
		adminSubjects[s] = struct{}{}
	}
}

// requireAdmin gates a request on the caller's subject being in the
// allowlist. Returns true when access is granted, otherwise writes a
// 403 and returns false.
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	sub, err := auth.SubjectFromContext(r.Context())
	if err != nil || sub == "" {
		http.Error(w, "no subject", http.StatusUnauthorized)
		return false
	}
	if _, ok := adminSubjects[sub]; !ok {
		http.Error(w, "admin access required", http.StatusForbidden)
		return false
	}
	return true
}

// postPackageRequest forwards POST /api/v1/admin/items/{id}/package to
// katalog-app's ItemActionsController.enqueuePackaging at
// /api/items/{id}/package. That endpoint sets the item's transcode
// step to 'pending', which the katalog-transcoder pod picks up via
// SELECT ... FOR UPDATE SKIP LOCKED; once transcode finishes the
// package step flips to 'pending' and the katalog-packager pods
// (4 replicas) take it from there. No more in-memory queue, no more
// single-worker bottleneck.
//
// Idempotent on the katalog side: re-POSTing for an item that's
// already transcoding/packaging/done is a no-op and returns the
// current status.
func postPackageRequest(katalogBase string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		id := chi.URLParam(r, "id")
		proxyToKatalog(w, r, katalogBase, http.MethodPost, "/api/items/"+url.PathEscape(id)+"/package")
	}
}

// getPackageStatus returns the step map for one item by forwarding to
// katalog-app's GET /api/analyze/items/{id}/steps. Response is the
// raw {step: status} map (e.g. {"transcode":"done","package":"in_progress"})
// — clients poll this to watch an item move through the pipeline.
func getPackageStatus(katalogBase string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r) {
			return
		}
		id := chi.URLParam(r, "id")
		proxyToKatalog(w, r, katalogBase, http.MethodGet, "/api/analyze/items/"+url.PathEscape(id)+"/steps")
	}
}

// proxyToKatalog forwards an admin request to katalog-app, carrying
// the caller's bearer through. katalog-app is the resource server for
// the same Keycloak realm, so the bearer the admin user sent us
// validates there too — no service-account swap needed.
func proxyToKatalog(w http.ResponseWriter, r *http.Request, base, method, path string) {
	if base == "" {
		http.Error(w, "katalog base not configured", http.StatusServiceUnavailable)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), method, base+path, http.NoBody)
	if err != nil {
		http.Error(w, "bad katalog url", http.StatusInternalServerError)
		return
	}
	// Forward the bearer. requireAdmin already validated it; the
	// inbound Authorization header is either "Bearer <jwt>" or a
	// ?token= we promoted earlier. Either way it lives on the
	// inbound request and katalog-app accepts the same JWT shape.
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "katalog upstream: "+err.Error(), http.StatusBadGateway)
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
