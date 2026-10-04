package auth

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

const key = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 bytes, base64

// A capped viewer's stream token carries its cap, signed with the rest; an
// uncapped viewer's carries none, as every token did before the caps.
func TestAStreamTokenCarriesTheCap(t *testing.T) {
	s, err := NewSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	uncapped, _ := s.Mint("user-1", time.Hour)
	twelve, _ := s.MintCapped("kid-1", 12, time.Hour)
	zero, _ := s.MintCapped("kid-2", 0, time.Hour)
	negative, _ := s.MintCapped("kid-3", -5, time.Hour)
	marked, _ := s.MintCapped("odd;max_rating=99", 6, time.Hour)
	for _, tc := range []struct {
		token, user, cap string
	}{
		{uncapped, "user-1", "none"},
		{twelve, "kid-1", "12"},
		{zero, "kid-2", "0"},
		{negative, "kid-3", "0"},
		{marked, "odd;max_rating=99", "6"},
	} {
		user, maxRating, err := s.VerifyCapped(tc.token)
		got := "none"
		if maxRating != nil {
			got = strconv.Itoa(*maxRating)
		}
		if err != nil || user != tc.user || got != tc.cap {
			t.Errorf("VerifyCapped: %q %s %v, want %q %s", user, got, err, tc.user, tc.cap)
		}
		if user, err := s.Verify(tc.token); err != nil || user != tc.user {
			t.Errorf("Verify: %q %v, want %q", user, err, tc.user)
		}
	}

	// The cap is signed: changed, the token is refused; expired, too.
	dot := strings.IndexByte(twelve, '.')
	payload, _ := base64.RawURLEncoding.DecodeString(twelve[:dot])
	forged := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(payload), "=12|", "=99|", 1))) + twelve[dot:]
	if _, _, err := s.VerifyCapped(forged); err == nil {
		t.Error("a token whose cap was changed was taken")
	}
	expired, _ := s.MintCapped("kid-1", 12, -time.Minute)
	if _, _, err := s.VerifyCapped(expired); err == nil {
		t.Error("an expired capped token was taken")
	}
}

// chino-stream and katalog-manager read a stream token's user part up to
// the first "|" and its expiry after it: a capped token reads to them as a
// token of a user whose name holds the cap, with the same expiry.
func TestACappedStreamTokenReadsToTheOtherVerifiersAsBefore(t *testing.T) {
	s, err := NewSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	tok, exp := s.MintCapped("kid-1", 12, time.Hour)
	payload, err := base64.RawURLEncoding.DecodeString(tok[:strings.IndexByte(tok, '.')])
	if err != nil {
		t.Fatal(err)
	}
	pipe := strings.IndexByte(string(payload), '|')
	expUnix, err := strconv.ParseInt(string(payload[pipe+1:]), 10, 64)
	if pipe < 1 || err != nil || expUnix != exp.Unix() || string(payload[:pipe]) != "kid-1;max_rating=12" {
		t.Errorf("the payload %q reads as user %q, expiry %v (%v), want kid-1;max_rating=12 and %d",
			payload, payload[:max(pipe, 0)], expUnix, err, exp.Unix())
	}
}
