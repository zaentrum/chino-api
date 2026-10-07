package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Signer mints and verifies short-lived signed stream tokens. These
// stand in for the OIDC access token on long-lived <video src> URLs:
// the OIDC token rotates every ~5 min via silent renew, which would
// otherwise change the play URL on every rotation, swap <video src>,
// close the HTTP connection, and kill the in-flight ffmpeg transcode.
// A stream token sticks around for hours so the URL stays stable
// across renewals.
//
// Token format (URL-safe; opaque to clients):
//
//	base64url(userID|expUnix) "." base64url(HMAC-SHA256(payload, key))
//
// Bound to a user, not to a specific item. The blast radius if a token
// leaks is "attacker can stream this user's media for the remaining
// TTL" — the same scope as a leaked OIDC access token, just with a
// longer expiry. Restricted server-side via StreamMiddleware to the media
// and artwork routes, the /events stream and the /play/events beacon, so
// it can't be used to poke at /me/* endpoints.
type Signer struct {
	key []byte
}

// NewSigner returns a Signer using the provided key, or generates a
// random 32-byte key when keyB64 is empty. A generated key is logged
// (without the value) as a warning: every process restart invalidates
// all in-flight tokens (mid-stream playbacks will need to re-mint).
// For production set STREAM_SIGNING_KEY to a stable base64 secret.
func NewSigner(keyB64 string) (*Signer, error) {
	if keyB64 == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate stream key: %w", err)
		}
		slog.Warn("STREAM_SIGNING_KEY unset; generated ephemeral key (tokens invalidate on restart)")
		return &Signer{key: key}, nil
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("decode STREAM_SIGNING_KEY (must be base64): %w", err)
	}
	if len(key) < 16 {
		return nil, errors.New("STREAM_SIGNING_KEY must be at least 16 bytes (base64-decoded)")
	}
	return &Signer{key: key}, nil
}

// Mint returns a signed token for userID valid until exp.
func (s *Signer) Mint(userID string, ttl time.Duration) (token string, exp time.Time) {
	return s.mint(userID, ttl)
}

// MintCapped returns a signed token for userID, a viewer capped at maxRating
// (the max_rating claim of its access token), valid until exp: the cap rides
// in the token's user part, "<userID>;max_rating=<age>", signed with the rest,
// so a request the token authorizes holds its viewer to the cap.
func (s *Signer) MintCapped(userID string, maxRating int, ttl time.Duration) (token string, exp time.Time) {
	if maxRating < 0 {
		maxRating = 0
	}
	return s.mint(userID+streamCapMark+strconv.Itoa(maxRating), ttl)
}

func (s *Signer) mint(userID string, ttl time.Duration) (token string, exp time.Time) {
	exp = time.Now().Add(ttl)
	payload := base64.RawURLEncoding.EncodeToString(
		[]byte(userID + "|" + strconv.FormatInt(exp.Unix(), 10)))
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + sig, exp
}

// Verify checks the token's signature and expiry, returning the
// embedded userID, without the cap a capped viewer's token carries.
// Constant-time signature comparison protects against timing oracles.
func (s *Signer) Verify(token string) (userID string, err error) {
	userID, _, err = s.VerifyCapped(token)
	return userID, err
}

// VerifyCapped is Verify with the rating cap the token carries: the age of a
// token MintCapped minted, nil for one Mint minted (an uncapped viewer's, and
// every token older than the caps).
func (s *Signer) VerifyCapped(token string) (userID string, maxRating *int, err error) {
	userID, err = s.verify(token)
	if err != nil {
		return "", nil, err
	}
	userID, maxRating = splitCap(userID)
	return userID, maxRating, nil
}

func (s *Signer) verify(token string) (userID string, err error) {
	dot := strings.IndexByte(token, '.')
	if dot < 1 || dot == len(token)-1 {
		return "", errors.New("malformed stream token")
	}
	payload, sig := token[:dot], token[dot+1:]

	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	expected := mac.Sum(nil)
	// Read strictly: the last character of a 32-byte MAC carries two bits the
	// MAC has none of, which a lenient decoder ignores, so four spellings of
	// one MAC would verify. Only the one Mint writes (those bits zero) does.
	got, decErr := base64.RawURLEncoding.Strict().DecodeString(sig)
	if decErr != nil {
		return "", errors.New("malformed stream token signature")
	}
	if !hmac.Equal(expected, got) {
		return "", errors.New("invalid stream token signature")
	}

	body, decErr := base64.RawURLEncoding.DecodeString(payload)
	if decErr != nil {
		return "", errors.New("malformed stream token payload")
	}
	pipe := strings.IndexByte(string(body), '|')
	if pipe < 1 {
		return "", errors.New("malformed stream token payload (no separator)")
	}
	userID = string(body[:pipe])
	expUnix, parseErr := strconv.ParseInt(string(body[pipe+1:]), 10, 64)
	if parseErr != nil {
		return "", errors.New("malformed stream token expiry")
	}
	if time.Now().Unix() > expUnix {
		return "", errors.New("stream token expired")
	}
	return userID, nil
}
