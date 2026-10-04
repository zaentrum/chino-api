package http

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/zaentrum/chino-api/internal/auth"
)

// claimCases are bearers' max_rating claims and the cap each holds its viewer
// to: the age of a whole number of years, none without the claim, and the
// strictest, 0, for any other value.
var claimCases = []struct {
	name  string
	claim any // nil: no claim at all
	want  string
}{
	{"no claim", nil, "uncapped"},
	{"12", 12, "12"},
	{"0", 0, "0"},
	{"18", 18, "18"},
	{"16.0", 16.0, "16"},
	{"12.5", 12.5, "0"},
	{"-1", -1, "0"},
	{`"12"`, "12", "0"},
	{"null", json.RawMessage("null"), "0"},
	{"true", true, "0"},
	{"[12]", []int{12}, "0"},
	{"an object", map[string]int{"age": 12}, "0"},
	{"1e300", 1e300, "0"},
}

// bearerWith is a bearer of is for sub with audience chino, whose
// max_rating claim is claim (none when nil).
func bearerWith(t *testing.T, is *issuer, sub string, claim any) http.Header {
	t.Helper()
	var extra map[string]any
	if claim != nil {
		extra = map[string]any{auth.MaxRatingClaim: claim}
	}
	return http.Header{"Authorization": {"Bearer " + is.tokenWith(t, sub, "chino", extra)}}
}

// The stream token a viewer mints carries the cap of its bearer's max_rating:
// none without the claim, the age of one, the strictest cap for a claim
// that is no whole number of years, said once.
func TestTheStreamTokenCarriesTheBearersCap(t *testing.T) {
	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	is := newIssuer(t)
	h := oidcRouter(t, is)
	signer, err := auth.NewSigner(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	malformed := 0
	for _, tc := range claimCases {
		w := do(h, "POST", "/api/v1/me/stream-token", bearerWith(t, is, "kid-1", tc.claim))
		var body struct {
			Token string `json:"stream_token"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body)
		}
		user, maxRating, err := signer.VerifyCapped(body.Token)
		got := "uncapped"
		if maxRating != nil {
			got = strconv.Itoa(*maxRating)
		}
		if err != nil || user != "kid-1" || got != tc.want {
			t.Errorf("a bearer with %s: the token's user %q cap %s (%v), want kid-1 at %s", tc.name, user, got, err, tc.want)
		}
		if tc.want == "0" && tc.name != "0" {
			malformed++
		}
	}
	if n := strings.Count(logged.String(), "no whole number of years"); malformed > 0 && n != 1 {
		t.Errorf("malformed claims said %d times, want once:\n%s", n, logged.String())
	}
}
