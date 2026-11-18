package auth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// testIssuer is an OIDC provider (discovery and keys) that signs RS256
// tokens. While down is set it answers its discovery 503; the first hang
// requests for its keys it holds unanswered until the asking client gives
// up, or the test ends.
type testIssuer struct {
	*httptest.Server
	rsaKey *rsa.PrivateKey
	down   atomic.Bool
	hang   atomic.Int32
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &testIssuer{rsaKey: k}
	ended := make(chan struct{})
	mux := http.NewServeMux()
	is.Server = httptest.NewServer(mux)
	t.Cleanup(is.Close)
	t.Cleanup(func() { close(ended) }) // before Close: a held request ends
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if is.down.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": is.URL, "jwks_uri": is.URL + "/keys",
			"authorization_endpoint": is.URL + "/auth", "token_endpoint": is.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		if is.hang.Add(-1) >= 0 {
			select {
			case <-r.Context().Done():
			case <-ended:
			}
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": enc(k.N.Bytes()), "e": enc(big.NewInt(int64(k.E)).Bytes()),
		}}})
	})
	return is
}

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// bearer is an Authorization header with an access token of is for sub.
func (is *testIssuer) bearer(t *testing.T, sub string) string {
	t.Helper()
	part := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return enc(b)
	}
	signed := part(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"}) + "." + part(map[string]any{
		"iss": is.URL, "sub": sub, "aud": "chino", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()})
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, is.rsaKey, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + signed + "." + enc(sig)
}

// testClock is the tests' clock: it moves when a test moves it.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// lockedLog is what the log package writes while a test runs.
type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.FieldsFunc(l.b.String(), func(r rune) bool { return r == '\n' })
}

func captureLog(t *testing.T) *lockedLog {
	t.Helper()
	l := &lockedLog{}
	prev := log.Writer()
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(prev) })
	return l
}

// status is what v's middleware answers a GET with authorization, under
// ctx: the route's 200 where the bearer verifies.
func status(ctx context.Context, v *Verifier, authorization string) int {
	h := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil).WithContext(ctx)
	r.Header.Set("Authorization", authorization)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// A discovery the issuer fails is not asked again within discoveryRetry:
// a bearer is answered its failure at once, with no second discovery; after
// it, a bearer has it asked again, and verifies once the issuer answers.
func TestAFailedDiscoveryIsAskedAgainASecondAfter(t *testing.T) {
	_ = captureLog(t)
	is := newTestIssuer(t)
	clock := newTestClock()
	var asked atomic.Int32
	v := NewVerifier(is.URL, "chino", true)
	v.now = clock.now
	v.ask = func(ctx context.Context, client *http.Client) (*oidc.IDTokenVerifier, error) {
		asked.Add(1)
		return v.askIssuer(ctx, client)
	}
	bearer := is.bearer(t, "viewer-1")

	is.down.Store(true)
	if got := status(context.Background(), v, bearer); got != http.StatusServiceUnavailable {
		t.Fatalf("the issuer down: %d, want 503", got)
	}
	failure := v.discovery.err
	is.down.Store(false)
	left, cancel := context.WithCancel(context.Background())
	cancel()
	for _, after := range []time.Duration{0, 500 * time.Millisecond, 499 * time.Millisecond} {
		clock.advance(after)
		// At once: even a caller who has left is answered the failure, not
		// its own context's end.
		if _, err := v.init(left); err != failure {
			t.Errorf("%v after the failure: %v, want the failure at once", after, err)
		}
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("within %v of the failure: %d discoveries, want 1", discoveryRetry, n)
	}

	clock.advance(time.Millisecond)
	if got := status(context.Background(), v, bearer); got != http.StatusOK {
		t.Errorf("%v after the failure, the issuer up: %d, want 200", discoveryRetry, got)
	}
	if n := asked.Load(); n != 2 {
		t.Errorf("after %v: %d discoveries, want 2", discoveryRetry, n)
	}
}

// A failed discovery is logged at most once in discoveryLogEvery, the next
// line counting the failures in between.
func TestAFailedDiscoveryIsLoggedOnceAMinute(t *testing.T) {
	logged := captureLog(t)
	clock := newTestClock()
	v := NewVerifier("http://issuer.invalid", "chino", true)
	v.now = clock.now
	v.ask = func(context.Context, *http.Client) (*oidc.IDTokenVerifier, error) {
		return nil, errors.New("503 Service Unavailable")
	}
	for i := 0; i < 60; i++ {
		if _, err := v.init(context.Background()); err == nil {
			t.Fatal("a discovery that fails answered no error")
		}
		clock.advance(discoveryRetry)
	}
	if lines := logged.lines(); len(lines) != 1 || !strings.Contains(lines[0], "the sign-in discovery of OIDC_ISSUER failed") {
		t.Errorf("60 failures in a minute: logged %q, want one line", lines)
	}
	if _, err := v.init(context.Background()); err == nil {
		t.Fatal("a discovery that fails answered no error")
	}
	if lines := logged.lines(); len(lines) != 2 || !strings.Contains(lines[1], "(and 59 times since the last line)") {
		t.Errorf("a minute on: logged %q, want a second line counting 59", lines)
	}
}

// A discovery that panics fails with the panic as its error, and the
// process goes on; one that ends with neither a verifier nor an error has
// failed too. Either is asked again after discoveryRetry.
func TestADiscoveryThatPanicsOrMakesNothingHasFailed(t *testing.T) {
	logged := captureLog(t)
	for _, c := range []struct {
		name string
		ask  func(context.Context, *http.Client) (*oidc.IDTokenVerifier, error)
		want string
	}{
		{"a panic", func(context.Context, *http.Client) (*oidc.IDTokenVerifier, error) { panic("boom") },
			"the sign-in discovery panicked: boom"},
		{"nothing", func(context.Context, *http.Client) (*oidc.IDTokenVerifier, error) { return nil, nil },
			"the sign-in discovery made no verifier"},
	} {
		clock := newTestClock()
		var asked atomic.Int32
		v := NewVerifier("http://issuer.invalid", "chino", true)
		v.now = clock.now
		v.ask = func(ctx context.Context, client *http.Client) (*oidc.IDTokenVerifier, error) {
			asked.Add(1)
			return c.ask(ctx, client)
		}
		if got := status(context.Background(), v, "Bearer x"); got != http.StatusServiceUnavailable {
			t.Errorf("%s: %d, want 503", c.name, got)
		}
		if _, err := v.init(context.Background()); err == nil || err.Error() != c.want {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
		clock.advance(discoveryRetry)
		_, _ = v.init(context.Background())
		if n := asked.Load(); n != 2 {
			t.Errorf("%s: %d discoveries after %v, want 2", c.name, n, discoveryRetry)
		}
	}
	if lines := logged.lines(); len(lines) != 2 {
		t.Errorf("logged %q, want a line for each", lines)
	}
}

// A fetch of the issuer's keys that the issuer does not answer ends with the
// issuer's time to answer: the next bearer has the keys asked for again, and
// verifies.
func TestAKeyFetchTheIssuerDoesNotAnswerEnds(t *testing.T) {
	_ = captureLog(t)
	is := newTestIssuer(t)
	is.hang.Store(1)
	v := NewVerifier(is.URL, "chino", true)
	v.timeout = 200 * time.Millisecond
	bearer := is.bearer(t, "viewer-1")
	for _, want := range []int{http.StatusUnauthorized, http.StatusOK} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		got := status(ctx, v, bearer)
		cancel()
		if got != want {
			t.Errorf("the keys held, then answered: %d, want %d", got, want)
		}
	}
}
