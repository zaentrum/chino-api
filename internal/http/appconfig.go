package http

import (
	"net/http"
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
func appConfig(cfg config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Non-secret + identical for every caller, so open CORS and a
		// short cache: it changes only when the deployment is reconfigured.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "public, max-age=300")
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
			},
		})
	}
}

// publicAPIBase reports the external "<origin>/api" this server is reached
// at. Prefers the configured canonical PublicBaseURL; otherwise derives the
// origin from the request, honouring the reverse-proxy X-Forwarded-Proto.
func publicAPIBase(cfg config.Config, r *http.Request) string {
	if cfg.PublicBaseURL != "" {
		return strings.TrimRight(cfg.PublicBaseURL, "/") + "/api"
	}
	scheme := "https"
	if xf := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); xf != "" {
		scheme = xf
	} else if r.TLS == nil {
		scheme = "http"
	}
	return scheme + "://" + r.Host + "/api"
}
