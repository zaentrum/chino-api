package auth

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
)

type ctxKey int

const (
	subjectKey ctxKey = iota
	rolesKey
	ratingKey
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
// by Signer. Used only where a client has to put its credential in a URL
// (the media and artwork routes, the /events stream, the /play/events
// beacon) so the long-lived stream token can't be exchanged into general
// /me/* access. Falls through to standard bearer / ?token= auth when
// ?stream= is absent or invalid.
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
				if uid, maxRating, err := v.signer.VerifyCapped(s); err == nil {
					ctx := context.WithValue(r.Context(), subjectKey, uid)
					if maxRating != nil {
						ctx = WithMaxRating(ctx, *maxRating)
					}
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
				// Invalid / expired — fall through to standard auth so
				// the player gets a clean 401 it can re-mint from.
			}
		}
		// DEPRECATED: the bearer in the `?token=` query string. It is how
		// <img src>, sendBeacon and the like authenticated before the
		// stream token covered them (StreamMiddleware). It stays accepted
		// for one more release so clients can move to ?stream= or a header,
		// then it goes: a bearer in a URL ends up in browser history and in
		// the logs of everything on the way. chino-api's own request log
		// shows it as ?token=REDACTED, which tells who still sends it.
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
		ctx = context.WithValue(ctx, rolesKey, realmRoles(tok))
		if age, capped := maxRatingOf(tok); capped {
			ctx = WithMaxRating(ctx, age)
		}
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

// realmRoles are the realm roles a verified access token carries, where the
// realm puts them: realm_access.roles, a list of strings. A token without
// them, or with them in any other shape, carries none.
func realmRoles(tok *oidc.IDToken) []string {
	var c struct {
		RealmAccess struct {
			Roles []string `json:"roles"`
		} `json:"realm_access"`
	}
	if err := tok.Claims(&c); err != nil {
		return nil
	}
	return c.RealmAccess.Roles
}

// RolesFromContext are the realm roles of the bearer a request was verified
// with; none for a stream token, which carries a subject alone, and none with
// OIDC off.
func RolesFromContext(ctx context.Context) []string {
	roles, _ := ctx.Value(rolesKey).([]string)
	return roles
}

// HasRole reports whether the request's verified bearer carries the realm
// role role; never for an empty role.
func HasRole(ctx context.Context, role string) bool {
	return role != "" && slices.Contains(RolesFromContext(ctx), role)
}
