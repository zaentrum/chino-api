package http

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/zaentrum/chino-api/internal/config"
)

// appConfig serves the unauthenticated, CORS-open discovery document that
// makes a chino server self-describing. A neutral self-host client that
// knows only the server URL GETs /api/config to learn the OIDC issuer +
// the public client id to use per platform, then runs OIDC discovery
// (.well-known/openid-configuration) against that issuer. The client ids
// here are the PUBLIC (no-secret) clients the operator must register in
// their IdP with the device-authorization grant + offline_access enabled.
// keepsSettings is whether this server keeps an account's settings: it has
// a database. With sign-in off (OIDC_ENABLED=false) a request has no
// account, and it keeps them for nobody.
func appConfig(cfg config.Config, keepsSettings bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Non-secret, so open CORS, and a short cache: it changes only when
		// the deployment is reconfigured. Identical for every caller where
		// PUBLIC_BASE_URL names the origin, and shared caches may keep it;
		// else apiBase is the request's own, and only the caller's may.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if cfg.PublicBaseURL != "" {
			w.Header().Set("Cache-Control", "public, max-age=300")
		} else {
			w.Header().Set("Cache-Control", "private, max-age=300")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"product":      "chino",
			"apiBase":      publicAPIBase(cfg, r),
			"oidcIssuer":   cfg.OIDCIssuer,
			"oidcAudience": cfg.OIDCAudience,
			"oidcEnabled":  cfg.OIDCEnabled,
			"oidcClientId": map[string]string{
				"tv":     cfg.OIDCClientIDTV,
				"mobile": cfg.OIDCClientIDMobile,
				"web":    cfg.OIDCClientIDWeb,
				"portal": cfg.OIDCClientIDPortal,
			},
			// What this server offers that a client should show only when
			// it does. feedback: POST /api/v1/feedback files bug reports
			// (it answers 503 when this is false). downloads: a phone may
			// keep a packaged title to play without a connection
			// (DOWNLOADS_ENABLED); a client offers no download while it is
			// false. watchlist_kinds: the account keeps whether its lists
			// show movies and shows apart (GET and PATCH
			// /api/v1/me/settings, watchlist_by_kind), and a list is
			// answered by kind (?kind=, ?include=counts); without a
			// database, or with sign-in off, nothing is kept, and a client
			// leaves the setting out.
			"features": map[string]bool{
				"feedback":        feedbackEnabled(cfg),
				"downloads":       cfg.DownloadsEnabled,
				"watchlist_kinds": keepsSettings && cfg.OIDCEnabled,
			},
		})
	}
}

// feedbackEnabled says whether bug reports can be filed: only with an
// OpenProject token configured. The router builds the OpenProject client on
// the same condition, so /api/config never promises what the endpoint
// refuses.
func feedbackEnabled(cfg config.Config) bool {
	return strings.TrimSpace(cfg.OpenProjectToken) != ""
}

// publicAPIBase reports the external "<origin>/api" this server is reached
// at. Prefers the configured canonical PublicBaseURL; otherwise derives the
// origin from the request: its Host where it is a host in its own syntax
// (hostSyntax: a name or an address, a port of 1 to 65535, no path, no
// userinfo), with the reverse proxy's X-Forwarded-Proto where it is http or
// https. A Host of any other form gives "/api", the API at the origin the
// caller reached.
func publicAPIBase(cfg config.Config, r *http.Request) string {
	if cfg.PublicBaseURL != "" {
		return strings.TrimRight(cfg.PublicBaseURL, "/") + "/api"
	}
	if !hostSyntax.MatchString(r.Host) {
		return "/api"
	}
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	if xf, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ","); strings.EqualFold(strings.TrimSpace(xf), "http") ||
		strings.EqualFold(strings.TrimSpace(xf), "https") {
		scheme = strings.ToLower(strings.TrimSpace(xf))
	}
	return scheme + "://" + r.Host + "/api"
}

// hostSyntax is a Host in its own syntax: a DNS name or an IPv4 address, or
// an IPv6 address in brackets, and a port of 1 to 65535.
var hostSyntax = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*|\[[0-9A-Fa-f:.]+\])` +
	`(?::(?:[1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5]))?$`)
