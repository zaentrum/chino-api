package auth

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
)

// A kid's account is capped at an age: its access token carries the claim
// max_rating, a whole number of years, and the kid is served no title rated
// above it. A token without the claim is not capped (an admin, an adult).
// One whose claim is no whole number of years is held to the strictest cap,
// 0, so a claim gone wrong shows less, never more, and chino-api says so
// once. A stream token minted for a capped viewer carries the cap, signed
// with the rest (Signer.MintCapped), so the media routes hold the viewer to
// it too.

// MaxRatingClaim is the claim of an access token that caps its viewer at an
// age.
const MaxRatingClaim = "max_rating"

var malformedCap sync.Once

// MaxRatingFromContext is the age the request's viewer is capped at: the
// max_rating claim of the bearer it was verified with, or the cap of its
// stream token; capped is false for a viewer without one, and with OIDC off.
func MaxRatingFromContext(ctx context.Context) (age int, capped bool) {
	age, capped = ctx.Value(ratingKey).(int)
	return age, capped
}

// WithMaxRating is ctx for a viewer capped at age.
func WithMaxRating(ctx context.Context, age int) context.Context {
	return context.WithValue(ctx, ratingKey, age)
}

// maxRatingOf reads the max_rating claim of a verified token: the age of a
// whole number of years; 0, said once, for any other value; capped false
// without the claim.
func maxRatingOf(tok *oidc.IDToken) (age int, capped bool) {
	var claims map[string]json.RawMessage
	if err := tok.Claims(&claims); err != nil {
		return 0, true // unreadable claims cap at the strictest age
	}
	raw, ok := claims[MaxRatingClaim]
	if !ok {
		return 0, false
	}
	if age, ok := wholeYears(raw); ok {
		return age, true
	}
	malformedCap.Do(func() {
		slog.Warn("a max_rating claim is no whole number of years: its viewer is held to the strictest cap, 0 (said once)",
			"sub", tok.Subject, "claim", string(raw))
	})
	return 0, true
}

// wholeYears reads a JSON value as a whole number of years: a number without
// a fraction, 0 or more. A string, a fraction, a negative number, null and
// anything else are not.
func wholeYears(raw json.RawMessage) (int, bool) {
	var f float64
	if len(raw) == 0 || raw[0] == '"' || string(raw) == "null" || json.Unmarshal(raw, &f) != nil {
		return 0, false
	}
	if f < 0 || f > math.MaxInt32 || f != math.Trunc(f) {
		return 0, false
	}
	return int(f), true
}

// streamCapMark joins the subject and the cap in the user part of a capped
// viewer's stream token: "<subject>;max_rating=<age>|<expiry>". The cap is
// read after the last mark, so a subject that holds the mark is still read
// whole. chino-stream takes the user part for an opaque word and
// katalog-manager reads the cap as chino-api does (their verifiers parse the
// expiry after the first "|", which the mark leaves where it was).
const streamCapMark = ";" + MaxRatingClaim + "="

// splitCap splits the user part of a stream token into the subject and the
// cap a capped viewer's token carries; nil when it carries none. A cap that
// is no whole number of years is the strictest, 0.
func splitCap(user string) (string, *int) {
	i := strings.LastIndex(user, streamCapMark)
	if i < 0 {
		return user, nil
	}
	age, err := strconv.Atoi(user[i+len(streamCapMark):])
	if err != nil || age < 0 {
		age = 0
	}
	return user[:i], &age
}
