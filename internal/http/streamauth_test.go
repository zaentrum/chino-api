package http

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
)

// issuer is a minimal OIDC provider (discovery + JWKS) signing RS256 access
// tokens, so the bearer paths run for real.
type issuer struct {
	*httptest.Server
	key *rsa.PrivateKey
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &issuer{key: key}
	mux := http.NewServeMux()
	is.Server = httptest.NewServer(mux)
	t.Cleanup(is.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": is.URL, "jwks_uri": is.URL + "/jwks",
			"authorization_endpoint": is.URL + "/auth", "token_endpoint": is.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	return is
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// token is an access token for sub with audience aud.
func (is *issuer) token(t *testing.T, sub, aud string) string {
	t.Helper()
	return is.tokenWith(t, sub, aud, nil)
}

// tokenWith is an access token for sub with audience aud, with the claims
// extra adds (realm_access, say).
func (is *issuer) tokenWith(t *testing.T, sub, aud string, extra map[string]any) string {
	t.Helper()
	part := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b64(b)
	}
	claims := map[string]any{"iss": is.URL, "sub": sub, "aud": aud, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	for k, v := range extra {
		claims[k] = v
	}
	signed := part(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"}) + "." + part(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, is.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + b64(sig)
}

// oidcRouter is chino-api with OIDC verified against is.
func oidcRouter(t *testing.T, is *issuer) http.Handler {
	t.Helper()
	h, err := NewRouter(config.Config{
		OIDCIssuer:       is.URL,
		OIDCAudience:     "chino",
		OIDCEnabled:      true,
		KatalogBaseURL:   "http://katalog-api.invalid",
		ArtworkBaseURL:   "http://katalog-manager.invalid",
		StreamBaseURL:    "http://chino-stream.invalid",
		StreamSigningKey: signingKey,
	}, nil, eventsse.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The telemetry beacon and the live-events stream take a stream token in
// place of the bearer in the URL; for this release the bearer still works
// either way (header or the deprecated ?token=). The stream token still
// opens nothing beyond the URL-only routes.
func TestURLOnlyRoutesTakeTheStreamToken(t *testing.T) {
	is := newIssuer(t)
	h := oidcRouter(t, is)
	bearer := is.token(t, "user-1", "chino")
	stream := streamToken(t) // user-1
	var telemetry bytes.Buffer
	log.SetOutput(&telemetry)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	beacon := func(query string, header http.Header) int {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/play/events"+query,
			strings.NewReader(`{"sessionId":"s1","events":[{"ts":1,"kind":"play","itemId":"i1"}]}`))
		for k, v := range header {
			r.Header[k] = v
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	events := func(query string, header http.Header) (int, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/events"+query, nil).WithContext(ctx)
		for k, v := range header {
			r.Header[k] = v
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r) // returns when ctx ends the stream
		return w.Code, w.Body.String()
	}
	authz := http.Header{"Authorization": {"Bearer " + bearer}}
	cases := []struct {
		name   string
		query  string
		header http.Header
		want   int
	}{
		{"stream token", "?stream=" + stream, nil, http.StatusOK},
		{"bearer header", "", authz, http.StatusOK},
		{"deprecated ?token=", "?token=" + bearer, nil, http.StatusOK},
		{"nothing", "", nil, http.StatusUnauthorized},
		{"a forged stream token", "?stream=" + stream[:len(stream)-2] + "xx", nil, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		wantBeacon := tc.want
		if wantBeacon == http.StatusOK {
			wantBeacon = http.StatusNoContent
		}
		if got := beacon(tc.query, tc.header); got != wantBeacon {
			t.Errorf("POST /play/events, %s: %d, want %d", tc.name, got, wantBeacon)
		}
		code, body := events(tc.query, tc.header)
		if code != tc.want || (code == http.StatusOK && !strings.HasPrefix(body, ": connected")) {
			t.Errorf("GET /events, %s: %d %q, want %d", tc.name, code, body, tc.want)
		}
	}
	if !strings.Contains(telemetry.String(), `"user":"user-1"`) {
		t.Errorf("telemetry lines carry the user from either credential:\n%s", telemetry.String())
	}

	// The stream token stays scoped: it opens no default-group route.
	if w := do(h, "GET", "/api/v1/me?stream="+stream, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("GET /me with a stream token: %d, want 401", w.Code)
	}
	if w := do(h, "GET", "/api/v1/me?token="+bearer, nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"sub":"user-1"`) {
		t.Errorf("GET /me with the deprecated ?token=: %d %s, want 200 for this release", w.Code, w.Body)
	}
}
