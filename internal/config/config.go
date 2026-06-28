package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Addr         string
	OIDCIssuer   string
	OIDCAudience string
	OIDCEnabled  bool

	// KatalogBaseURL is the in-cluster URL of katalog-api (Go read-only,
	// ADR-011 split, cloud_katalog_ro Postgres role). Owns the metadata
	// surface chino-api consumes: /api/v1/items, /movies, /series,
	// /episodes, /albums, /genres, /items/{id}, /series/{id}/episodes,
	// /items/{id}/segments, /items/{id}/asset.
	KatalogBaseURL string

	// StreamBaseURL is the in-cluster URL of chino-stream, the Go service
	// that handles HLS + trickplay + per-item /play/info. Distinct from
	// KatalogBaseURL — playback bytes never touch katalog-api.
	StreamBaseURL string

	// ArtworkBaseURL is the in-cluster URL of katalog-manager-api (the
	// CAP Java write surface). Artwork rows live in itemartworkdata
	// which is owned by the writer; katalog-api hasn't grown an artwork
	// endpoint yet. When it does, flip this to the same value as
	// KatalogBaseURL and the env var becomes a no-op.
	ArtworkBaseURL string

	// PgURL is the libpq-form Postgres URL chino-api uses for user-state
	// (playback progress + future watchlist / history). Same database as
	// the katalog service; different table prefix.
	// When empty, progress + telemetry endpoints respond gracefully but
	// don't persist — keeps local dev simple.
	PgURL string

	// AnalyzerBaseURL is the in-cluster URL of katalog-analyzer. The
	// admin packaging endpoint forwards POST/GET /api/v1/admin/items/
	// {id}/package here as POST/GET /api/package/{id}. Cluster-internal
	// only; no auth on the analyzer side beyond NetworkPolicy.
	AnalyzerBaseURL string

	// AdminSubjects is the comma-separated allowlist of Keycloak `sub`
	// values that may call POST /api/v1/admin/* endpoints (currently
	// just the packaging trigger). Empty list = nobody can; useful for
	// disabling the admin surface entirely in non-prod. Future: switch
	// to a role-claim check (realm_access.roles contains "admin") so
	// we don't have to redeploy on team changes.
	AdminSubjects []string

	// StreamSigningKey is the base64-encoded HMAC secret used to mint
	// and verify long-lived stream tokens that stand in for the OIDC
	// access token on <video src> URLs. Must be the SAME value in
	// chino-api and katalog-stream (the proxy forwards `?stream=` as-is
	// and katalog-stream re-verifies it). When empty, both services
	// generate ephemeral random keys at boot; URLs minted by one pod
	// won't validate at another, so the feature only works on a
	// single-pod deployment in that case. Production should set this
	// via a shared k8s Secret.
	StreamSigningKey string

	// OIDC client ids advertised by GET /api/config so a neutral self-host
	// client learns which PUBLIC (no-secret) OIDC client to use per
	// platform, then runs discovery against OIDCIssuer. The operator must
	// register these in their IdP with the device-authorization grant +
	// offline_access enabled. Default to the unified `chino` public client
	// (device-authorization + PKCE; its tokens carry aud=chino-web via an
	// audience mapper, so OIDCAudience stays chino-web). A deployment only
	// overrides a platform via env when it registers a distinct client.
	OIDCClientIDTV     string
	OIDCClientIDMobile string
	OIDCClientIDWeb    string

	// PublicBaseURL, when set, is the canonical external origin of this
	// chino-api (e.g. https://chino.example.com). GET /api/config reports
	// "<PublicBaseURL>/api" as apiBase; when empty it derives the origin
	// from the request (X-Forwarded-Proto + Host).
	PublicBaseURL string

	// OpenProject wiring for POST /api/v1/feedback (bug-report pipeline).
	// Reports become Bug work packages in the feedback project of your
	// OpenProject instance, created as a dedicated service user
	// (HTTP Basic "apikey:<token>"). When OpenProjectToken is empty the
	// feedback endpoint answers 503 and the clients treat the feature as
	// off — keeps local dev and self-host installs working untouched.
	OpenProjectURL       string
	OpenProjectToken     string
	OpenProjectProjectID int
	OpenProjectBugTypeID int
}

func Load() Config {
	c := Config{
		Addr:               envDefault("ADDR", ":8080"),
		OIDCIssuer:         envDefault("OIDC_ISSUER", "https://sso.example/realms/zaentrum"),
		OIDCAudience:       envDefault("OIDC_AUDIENCE", "chino-web"),
		OIDCEnabled:        envDefault("OIDC_ENABLED", "true") != "false",
		KatalogBaseURL:     envDefault("KATALOG_BASE_URL", "http://katalog-api.stube.svc.cluster.local"),
		StreamBaseURL:      envDefault("STREAM_BASE_URL", "http://chino-stream.stube.svc.cluster.local"),
		ArtworkBaseURL:     envDefault("ARTWORK_BASE_URL", "http://katalog-manager-api.stube.svc.cluster.local"),
		AnalyzerBaseURL:    envDefault("ANALYZER_BASE_URL", "http://katalog-manager-api.stube.svc.cluster.local"),
		AdminSubjects:      splitCSV(envDefault("ADMIN_SUBJECTS", "")),
		PgURL:              envDefault("PG_URL", ""),
		StreamSigningKey:   envDefault("STREAM_SIGNING_KEY", ""),
		OIDCClientIDTV:     envDefault("OIDC_CLIENT_ID_TV", "chino"),
		OIDCClientIDMobile: envDefault("OIDC_CLIENT_ID_MOBILE", "chino"),
		OIDCClientIDWeb:    envDefault("OIDC_CLIENT_ID_WEB", "chino"),
		PublicBaseURL:      envDefault("PUBLIC_BASE_URL", ""),

		OpenProjectURL:       envDefault("OPENPROJECT_URL", "https://openproject.example"),
		OpenProjectToken:     envDefault("OPENPROJECT_TOKEN", ""),
		OpenProjectProjectID: envInt("OPENPROJECT_PROJECT_ID", 36),
		OpenProjectBugTypeID: envInt("OPENPROJECT_BUG_TYPE_ID", 7),
	}
	return c
}

func envDefault(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

// envInt parses an integer env var, falling back to the default when
// unset or malformed — config never aborts boot on a bad number.
func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
