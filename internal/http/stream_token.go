package http

import (
	"net/http"
	"time"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/store"
)

// streamTokenTTL is the lifetime of a minted stream token. Long enough
// that an OIDC silent-renew (every ~5 min) plus a few network hiccups
// never invalidates an in-flight playback, short enough that a leaked
// token's blast radius is bounded.
const streamTokenTTL = 6 * time.Hour

// postStreamToken mints a stream token bound to the authenticated user.
// The player swaps the rotating OIDC access token out of <video src>
// for this stable token so token renewals no longer rotate the URL,
// kill the HTTP connection, and SIGKILL the in-flight ffmpeg.
//
// Bound to the user (not a specific item). Scoped server-side via
// StreamMiddleware to the URLs a client cannot put a header on — the
// media and artwork routes, the /events stream, the /play/events beacon
// — so a leaked token can stream the user's media (and post telemetry as
// them) but can't poke at /me/* or /items/* mutations.
//
// None is minted for an account whose deletion has begun: 401
// account_deleted, or 503 where it waited too long for that deletion to end
// (accountRefused). One minted before runs out by itself. Where the check
// itself fails - the database down - with the caller still there, it is
// logged and the token minted: a token takes no database, and without one
// the apps show no artwork and start no playback. Where the caller went
// (the request's context ended, the check with it), it is answered as a
// failed request, with no token.
func postStreamToken(signer *auth.Signer, st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		if err := st.CheckAccount(r.Context(), userID); err != nil {
			if accountRefused(w, r, err, "a stream token") {
				return
			}
			if r.Context().Err() != nil {
				storeFailed(w, r, err)
				return
			}
			logFailure(r, "the database", "the account's check, minted all the same", err)
		}
		// A capped viewer's token carries its cap, so the media routes hold
		// the viewer to it as the bearer routes do.
		token, exp := signer.Mint(userID, streamTokenTTL)
		if age, capped := auth.MaxRatingFromContext(r.Context()); capped {
			token, exp = signer.MintCapped(userID, age, streamTokenTTL)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"stream_token": token,
			"expires_at":   exp.UTC().Format(time.RFC3339),
		})
	}
}
