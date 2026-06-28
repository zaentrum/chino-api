package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
)

type ctxKey int

const (
	subjectKey ctxKey = iota
)

type Verifier struct {
	enabled  bool
	audience string
	once     sync.Once
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	initErr  error
	issuer   string
	signer   *Signer
}

func NewVerifier(issuer, audience string, enabled bool) *Verifier {
	return &Verifier{enabled: enabled, audience: audience, issuer: issuer}
}

// WithStreamSigner attaches a Signer so StreamMiddleware can accept
// `?stream=<signed-token>` as an alternative to the OIDC bearer.
// Returns the verifier for chaining.
func (v *Verifier) WithStreamSigner(s *Signer) *Verifier {
	v.signer = s
	return v
}

// Signer returns the attached stream-token signer (or nil).
func (v *Verifier) Signer() *Signer { return v.signer }

func (v *Verifier) init(ctx context.Context) error {
	v.once.Do(func() {
		p, err := oidc.NewProvider(ctx, v.issuer)
		if err != nil {
			v.initErr = err
			return
		}
		v.provider = p
		v.verifier = p.Verifier(&oidc.Config{ClientID: v.audience, SkipClientIDCheck: v.audience == ""})
	})
	return v.initErr
}

func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return v.middleware(next, false)
}

// StreamMiddleware additionally accepts `?stream=<signed-token>` minted
// by Signer. Used only on /play* routes so the long-lived stream token
// can't be exchanged into general /me/* access. Falls through to
// standard bearer / ?token= auth when ?stream= is absent or invalid.
func (v *Verifier) StreamMiddleware(next http.Handler) http.Handler {
	return v.middleware(next, true)
}

func (v *Verifier) middleware(next http.Handler, allowStream bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !v.enabled {
			next.ServeHTTP(w, r)
			return
		}
		// Stream token takes precedence on play routes: a stable URL
		// query param that survives OIDC silent-renew.
		if allowStream && v.signer != nil {
			if s := r.URL.Query().Get("stream"); s != "" {
				if uid, err := v.signer.Verify(s); err == nil {
					ctx := context.WithValue(r.Context(), subjectKey, uid)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
				// Invalid / expired — fall through to standard auth so
				// the player gets a clean 401 it can re-mint from.
			}
		}
		// `<video src>` and `<img src>` cannot set Authorization headers, so
		// for the streaming + artwork endpoints we also accept the bearer in
		// the `?token=` query string. Tradeoff: the token is visible in
		// access logs and the browser history.
		auth := r.Header.Get("Authorization")
		var raw string
		if strings.HasPrefix(auth, "Bearer ") {
			raw = strings.TrimPrefix(auth, "Bearer ")
		} else if t := r.URL.Query().Get("token"); t != "" {
			raw = t
			r.Header.Set("Authorization", "Bearer "+raw)
		} else {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		if err := v.init(r.Context()); err != nil {
			http.Error(w, "oidc provider unavailable", http.StatusServiceUnavailable)
			return
		}
		tok, err := v.verifier.Verify(r.Context(), raw)
		if err != nil {
			http.Error(w, "invalid token: "+err.Error(), http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), subjectKey, tok.Subject)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func SubjectFromContext(ctx context.Context) (string, error) {
	v, ok := ctx.Value(subjectKey).(string)
	if !ok || v == "" {
		return "", errors.New("no subject in context")
	}
	return v, nil
}
