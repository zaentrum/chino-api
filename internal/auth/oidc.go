package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/zaentrum/chino-api/internal/logline"
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
	issuer   string
	signer   *Signer

	// mu guards discovery, the last sign-in discovery asked of the issuer,
	// under way or ended (init), and the log of the failed ones: when the
	// last line was written, and how many failed since (logFailed).
	mu        sync.Mutex
	discovery *discovery
	loggedAt  time.Time
	unlogged  int

	// ask, timeout and now, where set, stand in for askIssuer, issuerTimeout
	// and time.Now: the tests' issuer, its time to answer, and their clock.
	ask     func(ctx context.Context, client *http.Client) (*oidc.IDTokenVerifier, error)
	timeout time.Duration
	now     func() time.Time
}

// discovery is one sign-in discovery of the issuer: done is closed when it
// has ended, at ended, with the verifier it made or the error it failed with.
type discovery struct {
	done     chan struct{}
	ended    time.Time
	verifier *oidc.IDTokenVerifier
	err      error
}

const (
	// issuerTimeout is how long the issuer has to answer a sign-in
	// discovery, and each fetch of its keys after it.
	issuerTimeout = 10 * time.Second
	// discoveryRetry is how long after a failed discovery the next is asked
	// for: until then a bearer is answered that failure at once.
	discoveryRetry = time.Second
	// discoveryLogEvery is how often a failed discovery is logged at most;
	// the line counts the failures since the last one.
	discoveryLogEvery = time.Minute
)

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

// init is the verifier of the issuer's sign-in discovery, waited for under
// ctx: the one discovery made, else the one under way, else the last one's
// failure while it is younger than discoveryRetry, else a new one. A
// discovery runs on a context of its own, so that a caller's wait ends with
// the caller alone; one that fails is not kept beyond discoveryRetry.
func (v *Verifier) init(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	v.mu.Lock()
	d := v.discovery
	if d == nil || d.failed() && v.clock().Sub(d.ended) >= discoveryRetry {
		d = &discovery{done: make(chan struct{})}
		v.discovery = d
		go v.discover(d)
	}
	v.mu.Unlock()
	// A discovery that has ended is the answer, also to a caller who has
	// left: its request goes on as it did before there was one to wait for.
	select {
	case <-d.done:
		return d.verifier, d.err
	default:
	}
	select {
	case <-d.done:
		return d.verifier, d.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// discover asks the issuer for its sign-in discovery, d, on a context of its
// own with issuerTimeout. Its panic is its error, and so is an end with no
// verifier; a failure is logged (logFailed).
func (v *Verifier) discover(d *discovery) {
	defer close(d.done)
	defer func() {
		if p := recover(); p != nil {
			d.verifier, d.err = nil, fmt.Errorf("the sign-in discovery panicked: %v", p)
		}
		if d.err == nil && d.verifier == nil {
			d.err = errors.New("the sign-in discovery made no verifier")
		}
		d.ended = v.clock()
		if d.err != nil {
			v.logFailed(d.err)
		}
	}()
	timeout := v.timeout
	if timeout == 0 {
		timeout = issuerTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ask := v.ask
	if ask == nil {
		ask = v.askIssuer
	}
	d.verifier, d.err = ask(ctx, &http.Client{Timeout: timeout})
}

// askIssuer is the verifier of the issuer's sign-in discovery, asked under
// ctx with client, which the provider keeps for every fetch of the issuer's
// keys after it: a fetch the issuer does not answer ends with the client's
// timeout, and the next bearer has the keys asked for again.
func (v *Verifier) askIssuer(ctx context.Context, client *http.Client) (*oidc.IDTokenVerifier, error) {
	p, err := oidc.NewProvider(oidc.ClientContext(ctx, client), v.issuer)
	if err != nil {
		return nil, err
	}
	return p.Verifier(&oidc.Config{ClientID: v.audience, SkipClientIDCheck: v.audience == ""}), nil
}

// logFailed logs err, a failed discovery's, where no line of one was written
// within discoveryLogEvery, with how many failed since the last; else it
// counts it for the next line.
func (v *Verifier) logFailed(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.clock()
	if !v.loggedAt.IsZero() && now.Sub(v.loggedAt) < discoveryLogEvery {
		v.unlogged++
		return
	}
	since := ""
	if v.unlogged > 0 {
		since = fmt.Sprintf(" (and %d times since the last line)", v.unlogged)
	}
	log.Printf("chino-api: the sign-in discovery of OIDC_ISSUER failed, asked again with a bearer %s after: %s%s",
		discoveryRetry, logline.Quote(err.Error()), since)
	v.loggedAt, v.unlogged = now, 0
}

// clock is the time, the tests' where they set now.
func (v *Verifier) clock() time.Time {
	if v.now != nil {
		return v.now()
	}
	return time.Now()
}

// failed reports whether d has ended without a verifier.
func (d *discovery) failed() bool {
	select {
	case <-d.done:
		return d.err != nil || d.verifier == nil
	default:
		return false
	}
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
		verifier, err := v.init(r.Context())
		if err != nil {
			http.Error(w, "oidc provider unavailable", http.StatusServiceUnavailable)
			return
		}
		tok, err := verifier.Verify(r.Context(), raw)
		if err != nil {
			log.Printf("chino-api: %s %s: a bearer was refused: %s", r.Method, logline.Route(r), logline.Quote(err.Error()))
			http.Error(w, refusal(err), http.StatusUnauthorized)
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

// The answers to a bearer the verifier refuses, with 401: a fixed line,
// whatever the reason, which is logged on the server. One made for another
// audience than OIDC_AUDIENCE is answered as such, so that an app can say
// that this server does not take the sign-in it used.
const (
	invalidToken  = "invalid token"
	otherAudience = "invalid token: wrong audience"
)

// refusal is the answer to a bearer the verifier refused with err. go-oidc
// says an audience it does not expect in no error of its own, only by the
// first words of the error's text.
func refusal(err error) string {
	if strings.HasPrefix(err.Error(), "oidc: expected audience ") {
		return otherAudience
	}
	return invalidToken
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
