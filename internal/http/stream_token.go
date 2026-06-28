package http

import (
	"net/http"
	"time"

	"github.com/zaentrum/chino-api/internal/auth"
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
// StreamMiddleware to /items/{id}/play* routes — a leaked token can
// stream the user's media but can't poke at /me/* or /items/*
// mutations.
func postStreamToken(signer *auth.Signer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.SubjectFromContext(r.Context())
		if userID == "" {
			http.Error(w, "no subject", http.StatusUnauthorized)
			return
		}
		token, exp := signer.Mint(userID, streamTokenTTL)
		writeJSON(w, http.StatusOK, map[string]any{
			"stream_token": token,
			"expires_at":   exp.UTC().Format(time.RFC3339),
		})
	}
}
