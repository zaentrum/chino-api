package http

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
	"github.com/zaentrum/chino-api/internal/katalog"
	"github.com/zaentrum/chino-api/internal/metrics"
	"github.com/zaentrum/chino-api/internal/openproject"
	"github.com/zaentrum/chino-api/internal/portal"
	"github.com/zaentrum/chino-api/internal/store"
)

func NewRouter(cfg config.Config, st *store.Store, events *eventsse.Broker) (http.Handler, error) {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	// middleware.Logger, with ?token= / ?stream= values blanked out.
	r.Use(requestLogger(os.Stdout))
	r.Use(middleware.Recoverer)
	r.Use(metrics.Middleware)
	// NB: middleware.Timeout is NOT applied at the top level — it would
	// kill long-lived /play transcodes mid-stream. Applied per-group
	// below so non-streaming routes still get a sane safety cap.

	r.Get("/api/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "product": "chino"})
	})

	r.Get("/api/openapi.yaml", serveOpenAPI)

	// Unauthenticated, CORS-open discovery doc so a neutral self-host
	// client that knows only the server URL can learn the OIDC issuer +
	// public client ids and bootstrap sign-in. See appconfig.go.
	r.Get("/api/config", appConfig(cfg))

	// Prometheus scrape target — un-authed because the chino-api Service
	// is cluster-internal; only the in-cluster hyperv-prometheus reaches
	// it (and chino-beta's network policy gates ingress).
	r.Method("GET", "/metrics", metrics.Handler())

	signer, err := auth.NewSigner(cfg.StreamSigningKey)
	if err != nil {
		return nil, err
	}
	verifier := auth.NewVerifier(cfg.OIDCIssuer, cfg.OIDCAudience, cfg.OIDCEnabled).WithStreamSigner(signer)
	kc := katalog.New(cfg.KatalogBaseURL)
	kc.StreamBaseURL = cfg.StreamBaseURL
	kc.ArtworkBaseURL = cfg.ArtworkBaseURL
	// streamKC keeps the same shape but its BaseURL points directly at
	// chino-stream so /api/play/* proxy calls don't have to re-route
	// through ProxyStream's path-prefix check. PlayInfoDurationMs reads
	// StreamBaseURL off the client struct so a single field is plenty.
	streamKC := katalog.New(cfg.StreamBaseURL)
	streamKC.StreamBaseURL = cfg.StreamBaseURL
	// Portal client for addon UI-extension slots (best-effort; empty when the
	// portal is unset/unreachable, so a no-addon instance shows no extra UI).
	pc := portal.New(cfg.PortalBaseURL)
	// OpenProject client for the bug-report pipeline. Nil when no token
	// is configured — postFeedback answers 503 and the clients treat
	// the feature as off.
	var op *openproject.Client
	if cfg.OpenProjectToken != "" {
		op = openproject.New(cfg.OpenProjectURL, cfg.OpenProjectToken, cfg.OpenProjectProjectID, cfg.OpenProjectBugTypeID)
	}

	// Every route of one title holds a capped viewer to its cap: a title the
	// cap does not allow is 404 there, as one there is not (ratings.go).
	g := gate{kc: kc}
	title, item := g.title("id"), g.title("itemId")

	r.Route("/api/v1", func(r chi.Router) {
		// Default group: OIDC bearer (header or the deprecated ?token=).
		// Stream tokens are NOT accepted here — they're scoped to the
		// media routes, /events and /play/events, so a leaked stream
		// token can't be exchanged for /me/watched or /items/*
		// mutations. 2-minute timeout cap keeps non-stream routes from
		// hanging on katalog upstreams.
		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(2 * time.Minute))
			r.Use(verifier.Middleware)
			r.Get("/me", whoAmI)
			// The signed-in person deletes their account: what chino keeps
			// of them here, then the account through portal-api (account.go).
			r.Delete("/me", deleteMe(st, pc, cfg.AccountDeletionToken))
			r.Get("/me/continue-watching", continueWatching(st, kc))
			// Mints a long-lived stream token (TTL 6 h) the player
			// uses on <video src> URLs so OIDC silent-renew can't
			// kill the in-flight ffmpeg transcode by rotating the URL.
			r.Post("/me/stream-token", postStreamToken(signer))
			r.Get("/items", listItems(st, kc))
			r.With(title).Get("/items/{id}", itemDetail(st, kc))
			r.With(title).Post("/me/items/{id}/watched", postWatched(st))
			r.With(title).Delete("/me/items/{id}/watched", deleteWatched(st))
			r.Get("/me/watched", listWatched(st, kc))
			r.With(title).Get("/items/{id}/segments", itemSegments(kc, streamKC))
			r.With(title).Get("/items/{id}/similar", similarItems(st, kc))
			// People search + filmography (search an actor → see their
			// films). Proxies katalog-api; the filmography items get the
			// same poster + watched_at enrichment as the browse lists.
			r.Get("/people", searchPeople(kc))
			r.Get("/people/{id}", getPerson(st, kc))
			r.With(title).Get("/series/{id}/episodes", seriesEpisodes(kc, st))
			r.With(title).Get("/series/{id}/next-episode", nextEpisode(kc, st))
			r.Get("/genres", listGenres(kc))
			r.With(title).Get("/items/{id}/subtitles", subtitlesList(kc))

			// Addon UI-extension slots: the SPA asks for the contributions
			// for a named slot (e.g. search.empty) and renders them natively.
			// Empty for a no-addon instance.
			r.Get("/extensions", listExtensions(pc))

			// Notices addons left the signed-in person, kept by portal-api
			// and forwarded with their bearer — best effort, like the slots:
			// the list is empty, with available false, when portal-api does
			// not answer (notices.go).
			r.Get("/notices", listNotices(pc))
			r.Post("/notices/read-all", readAllNotices(pc))
			r.Post("/notices/{noticeId}/read", readNotice(pc))
			r.Delete("/notices/{noticeId}", deleteNotice(pc))

			// User-state endpoints (chino-api owns these, not katalog-stream).
			// Resume position: GET returns the last saved second; POST writes
			// it. The player calls POST every ~10s while watching and GET
			// once on mount to decide whether to offer "Resume from X:YZ?".
			r.With(title).Get("/items/{id}/progress", getProgress(st))
			r.With(title).Post("/items/{id}/progress", postProgress(st))

			// Named watchlists — the user can keep several lists, each a
			// grid of items, with exactly one default named "Watchlist".
			// The picker on the detail page toggles item membership per
			// list; memberships hydrates the checkmarks + card "saved"
			// badge in one round-trip. (memberships is registered before
			// the {listId} routes so chi matches the static segment.)
			r.Get("/me/watchlists", listWatchlists(st, g))
			r.Post("/me/watchlists", createWatchlist(st))
			r.Get("/me/watchlists/memberships", watchlistMemberships(st, g))
			r.Get("/me/watchlists/{listId}", getWatchlist(st, g))
			r.Patch("/me/watchlists/{listId}", renameWatchlist(st))
			r.Delete("/me/watchlists/{listId}", deleteWatchlist(st))
			r.With(item).Put("/me/watchlists/{listId}/items/{itemId}", setWatchlistItem(st, true))
			r.With(item).Delete("/me/watchlists/{listId}/items/{itemId}", setWatchlistItem(st, false))

			// Back-compat watchlist routes — UNCHANGED response shapes for
			// already-installed mobile/TV builds, but now backed by the
			// user's default list (resolved/created via EnsureDefaultList)
			// instead of the legacy single watchlist flag table.
			r.Get("/me/watchlist", defaultListGet(st, g))
			r.With(title).Put("/me/watchlist/{id}", defaultListSet(st, true))
			r.With(title).Delete("/me/watchlist/{id}", defaultListSet(st, false))

			// Likes stay on the simple per-user flag table — unchanged.
			likesSpec := flagSpec{table: store.LikesTable, field: "liked"}
			r.Get("/me/likes", flagList(st, likesSpec, g))
			r.With(title).Put("/me/likes/{id}", flagSet(st, likesSpec, true))
			r.With(title).Delete("/me/likes/{id}", flagSet(st, likesSpec, false))

			// Bug-report intake. Multipart (report JSON + optional
			// screenshot) → OpenProject Bug work package, with
			// fingerprint dedup + per-user rate limiting. Clients
			// silently drop auto reports on any non-2xx; manual
			// reports surface errors in the dialog.
			r.Post("/feedback", postFeedback(st, op))

			// Admin: an item's packaging, and its steps to watch it by,
			// forwarded to katalog-manager (the catalog's writer, not the
			// read-only katalog-api) with the admin's bearer
			// (admin_package.go). For a bearer with the admin role; the
			// check is in the handlers, so the rest of the chain needs no
			// role-aware auth.
			admin := newAdminAccess(cfg.AdminRole, cfg.AdminSubjects)
			r.With(title).Post("/admin/items/{id}/package", postPackageRequest(admin, cfg.KatalogManagerURL))
			r.With(title).Get("/admin/items/{id}/package", getPackageStatus(admin, cfg.KatalogManagerURL))
			// Admin: what chino keeps of an account an admin deletes on the
			// portal's People page, for portal-api with the account deletion
			// token (account.go).
			r.Delete("/admin/accounts/{sub}/data", deleteAccountData(st, admin, cfg.AccountDeletionToken))
		})

		// Live catalog stream (SSE), in its OWN group — the default
		// group's 2-minute timeout would sever long-lived streams (the
		// server's WriteTimeout is already 0 for exactly this reason).
		// Bearer or stream token: an EventSource cannot set a header,
		// and a ?stream= URL survives silent renews where a ?token= one
		// changes (and reconnects) with every renewal. The events are
		// thin catalog notifications, nothing of the user's.
		r.Group(func(r chi.Router) {
			r.Use(verifier.StreamMiddleware)
			r.Get("/events", events.Handler)
		})

		// Telemetry sink. The player batches events (play / pause / seek
		// / waiting / stalled / error / quality switch / network rate)
		// and POSTs them every ~30 s + on `pagehide`. Server logs each
		// event as a structured JSON line for the cluster aggregator.
		// Bearer or stream token: the pagehide flush goes out with
		// navigator.sendBeacon, which cannot set a header either, and the
		// sink only logs and counts — no user state to protect.
		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(2 * time.Minute))
			r.Use(verifier.StreamMiddleware)
			r.Post("/play/events", postTelemetry)
		})

		// Play + media-asset group: stream token accepted alongside the
		// bearer. The stream token is the one credential that survives
		// an OIDC silent-renew without changing the URL — also used on
		// poster / backdrop <img src> URLs so the browser doesn't
		// reload every image on every renewal.
		r.Group(func(r chi.Router) {
			r.Use(verifier.StreamMiddleware)
			// Artwork: served from the stream group so <img src=...>
			// URLs stay stable across silent renews. With the OIDC
			// token in the URL, every renewal rotated the URL and the
			// browser fetched every poster + backdrop again — noisy
			// flicker on the home grid every 5 minutes.
			r.With(title).Get("/items/{id}/poster", proxyArtwork(kc, "poster"))
			r.With(title).Get("/items/{id}/backdrop", proxyArtwork(kc, "backdrop"))
			// A person's portrait — what a person's profile_url points at.
			r.Get("/people/{id}/profile", proxyPersonProfile(kc))
			// Legacy progressive-MP4 stream — kept for fallback and for
			// /info codec/probe discovery. The HLS endpoints below are
			// what chino-web uses for playback now.
			r.With(title).Get("/items/{id}/play", proxyPlay(streamKC))
			r.With(title).Get("/items/{id}/play/info", proxyPlayInfo(streamKC))
			// HLS pipeline: master playlist, per-quality media playlist,
			// init segment, on-demand media segments. The proxy
			// preserves query strings (?stream=...) so the same stream
			// token authorises every segment request without rewriting
			// URLs on each level.
			r.With(title).Get("/items/{id}/play/master.m3u8", proxyHLS(streamKC, "master.m3u8"))
			// Zap pager fires this for distance=1 cards. chino-stream
			// returns 202 immediately and warms window 0 in a
			// background goroutine off a dedicated ffmpeg pool.
			r.With(title).Post("/items/{id}/play/prewarm", proxyHLS(streamKC, "prewarm"))
			// Listing of items that already have a finished CMAF
			// package on disk. Used by the Zap pager to filter its
			// candidate pool to instant-start items (packaged items
			// skip ffmpeg, serve in <50ms). Proxies straight through
			// to chino-stream which owns the on-disk truth; a capped
			// viewer's leaves out what its cap does not allow.
			r.Get("/play/packaged-ids", proxyPackagedIDs(streamKC, g))
			// Zap warm-pool feed. chino-stream maintains a small
			// in-RAM pool of speculatively pre-warmed candidates;
			// chino-web's useZapFeed consumes the head of the pool
			// instead of building a cold candidate set per session. A
			// capped viewer's leaves out the cards its cap does not
			// allow.
			r.Get("/play/zap-feed", proxyZapFeed(streamKC, g))
			r.With(title).Get("/items/{id}/play/{quality}/index.m3u8", proxyHLSQ(streamKC, itemUpstream, "index.m3u8"))
			r.With(title).Get("/items/{id}/play/{quality}/init.mp4", proxyHLSQ(streamKC, itemUpstream, "init.mp4"))
			r.With(title).Get("/items/{id}/play/{quality}/{seg:[0-9]+}.m4s", proxyHLSSegment(streamKC, itemUpstream))
			// Audio rendition group (multi-language audio).
			r.With(title).Get("/items/{id}/play/audio/{audioIdx:[0-9]+}/index.m3u8", proxyHLSAudio(streamKC, itemUpstream, "index.m3u8"))
			r.With(title).Get("/items/{id}/play/audio/{audioIdx:[0-9]+}/init.mp4", proxyHLSAudio(streamKC, itemUpstream, "init.mp4"))
			r.With(title).Get("/items/{id}/play/audio/{audioIdx:[0-9]+}/{seg:[0-9]+}.m4s", proxyHLSAudioSegment(streamKC, itemUpstream))
			// Packaged-CMAF rendition routes. Rend IDs are v0/v1/.../
			// a0/a1/... — strict regex stops collisions with the
			// legacy {quality} routes above. The master playlist
			// served by katalog-stream points the player at these
			// when the item has been operator-packaged.
			r.With(title).Get("/items/{id}/play/{rendId:[va][0-9]+}/playlist.m3u8", proxyPackagedRendition(streamKC, "playlist.m3u8"))
			r.With(title).Get("/items/{id}/play/{rendId:[va][0-9]+}/iframes.m3u8", proxyPackagedRendition(streamKC, "iframes.m3u8"))
			r.With(title).Get("/items/{id}/play/{rendId:[va][0-9]+}/init.mp4", proxyPackagedRendition(streamKC, "init.mp4"))
			r.With(title).Get("/items/{id}/play/{rendId:[va][0-9]+}/seg-{seg:[0-9]+}.m4s", proxyPackagedSegment(streamKC))
			// Packaged WebVTT subtitle renditions (sN): the media playlist
			// and its seg-NNNNN.vtt segments, what a master's
			// TYPE=SUBTITLES group points at (the packager's
			// HLS_SUBTITLES).
			r.With(title).Get("/items/{id}/play/{rendId:s[0-9]+}/playlist.m3u8", proxyPackagedRendition(streamKC, "playlist.m3u8"))
			r.With(title).Get("/items/{id}/play/{rendId:s[0-9]+}/seg-{seg:[0-9]+}.vtt", proxyPackagedSubtitleSegment(streamKC))
			// Trickplay scrub-preview thumbnails. VTT + JPG sprite
			// sheets; the player loads these into hls.js's trickplay
			// hook (or directly via the seek-bar hover UI).
			r.With(title).Get("/items/{id}/play/trickplay/thumbnails.vtt", proxyTrickplayVTT(streamKC))
			r.With(title).Get("/items/{id}/play/trickplay/sprite-{n:[0-9]+}.jpg", proxyTrickplaySprite(streamKC))
			// A title's extras (its trailers, teasers, featurettes, …
			// packaged apart from it): an extra's master, its play_path in
			// the item detail, and the renditions the master names,
			// proxied to chino-stream's /api/play/{id}/extras/{extraId}/,
			// query and all. The title gate holds a capped viewer to the
			// rating of the title {id}; chino-stream serves an extra only
			// under its own title. An extra has no progress, watched,
			// segments, trickplay, /info or /prewarm.
			r.With(title).Get("/items/{id}/extras/{extraId}/play/master.m3u8", proxyExtra(streamKC, "master.m3u8"))
			r.With(title).Get("/items/{id}/extras/{extraId}/play/{rendId:[vas][0-9]+}/playlist.m3u8", proxyExtraRendition(streamKC, "playlist.m3u8"))
			r.With(title).Get("/items/{id}/extras/{extraId}/play/{rendId:[va][0-9]+}/iframes.m3u8", proxyExtraRendition(streamKC, "iframes.m3u8"))
			r.With(title).Get("/items/{id}/extras/{extraId}/play/{rendId:[va][0-9]+}/init.mp4", proxyExtraRendition(streamKC, "init.mp4"))
			r.With(title).Get("/items/{id}/extras/{extraId}/play/{rendId:[va][0-9]+}/seg-{seg:[0-9]+}.m4s", proxyExtraSegment(streamKC, ".m4s"))
			r.With(title).Get("/items/{id}/extras/{extraId}/play/{rendId:s[0-9]+}/seg-{seg:[0-9]+}.vtt", proxyExtraSegment(streamKC, ".vtt"))
			// An extra is packaged HEVC only, as a title is: for a client that
			// decodes none of its rungs chino-stream's master of it is an
			// on-the-fly one, its rung and audio tracks transcoded from the
			// package under the extra's routes. Proxied as a title's
			// on-the-fly ladder is, with the rungs chino-stream has; the
			// group keeps chi's ^…$ around all three (^high|medium|low$
			// would take highx or xlow).
			r.With(title).Get("/items/{id}/extras/{extraId}/play/{quality:(high|medium|low)}/index.m3u8", proxyHLSQ(streamKC, extraUpstream, "index.m3u8"))
			r.With(title).Get("/items/{id}/extras/{extraId}/play/{quality:(high|medium|low)}/init.mp4", proxyHLSQ(streamKC, extraUpstream, "init.mp4"))
			r.With(title).Get("/items/{id}/extras/{extraId}/play/{quality:(high|medium|low)}/{seg:[0-9]+}.m4s", proxyHLSSegment(streamKC, extraUpstream))
			r.With(title).Get("/items/{id}/extras/{extraId}/play/audio/{audioIdx:[0-9]+}/index.m3u8", proxyHLSAudio(streamKC, extraUpstream, "index.m3u8"))
			r.With(title).Get("/items/{id}/extras/{extraId}/play/audio/{audioIdx:[0-9]+}/init.mp4", proxyHLSAudio(streamKC, extraUpstream, "init.mp4"))
			r.With(title).Get("/items/{id}/extras/{extraId}/play/audio/{audioIdx:[0-9]+}/{seg:[0-9]+}.m4s", proxyHLSAudioSegment(streamKC, extraUpstream))
			// Embedded-subtitle stream: extracted on demand by
			// katalog-stream (ffmpeg -c:s webvtt). Proxied here so the
			// player can append ?stream=… and the browser's <track src>
			// works without CORS.
			r.With(title).Get("/items/{id}/play/subtitles/{streamIndex}.vtt", proxyEmbeddedSubtitle(streamKC))
			// Sidecar (pre-packaged) subtitle file. The subtitles list
			// handler above synthesises URLs that point here; the player
			// then mounts them as <track src>. Stream-token auth so the
			// browser's <track> requests work without OIDC headers.
			r.With(g.subtitle).Get("/play/subs/{id}.vtt", proxySidecarSubtitle(streamKC))
		})
	})

	return r, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func whoAmI(w http.ResponseWriter, r *http.Request) {
	sub, err := auth.SubjectFromContext(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"sub": sub})
}

// listItems queries one of katalog's entity sets based on `?type=`.
//
// When katalog fails it returns an ERROR. It used to answer HTTP 200 with four
// hardcoded sample films and source:"fallback" — so a catalog outage looked to
// every client like a working catalog containing titles that do not exist.
// chino-web filtered them by source=="katalog"; no other client did, so the TV
// and mobile apps rendered fabricated content as real. An empty rail or an
// explicit error is honest; invented content is not.
func listItems(st *store.Store, kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bearer := bearerFrom(r)
		q := r.URL.Query().Get("q")
		typ := r.URL.Query().Get("type")
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		offset := 0
		if v := r.URL.Query().Get("offset"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				offset = n
			}
		}
		extra := buildBrowseFilter(r)
		userID, _ := auth.SubjectFromContext(r.Context())

		// fetchPage runs one katalog query for the active type. Albums
		// ignore offset/extra (no filtered endpoint) — fine, they don't
		// use the unwatched path.
		fetchPage := func(lim, off int) ([]katalog.Item, error) {
			switch typ {
			case "series":
				return kc.ListSeriesFiltered(r.Context(), bearer, q, lim, off, extra)
			case "episode":
				// Flat episode listing (katalog's /episodes set). Without
				// this, type=episode fell through to the movies default —
				// so Zap's "episode" pull silently returned movies and the
				// 12k+ packaged episodes never reached the feed.
				return kc.ListEpisodesFiltered(r.Context(), bearer, q, lim, off, extra)
			case "album":
				return kc.ListAlbums(r.Context(), bearer, q, lim)
			default:
				return kc.ListMoviesFiltered(r.Context(), bearer, q, lim, off, extra)
			}
		}

		// Home rails pass ?unwatched=true: drop items the user already
		// finished and BACKFILL from later pages so the rail still returns
		// up to `limit` unwatched items (#189 — "fill it up to 20 again
		// with the next items"). Browse pages omit it so a watched title
		// stays findable for a rewatch. Over-fetch in bounded pages; a
		// rail capped at 20 against a mostly-unwatched library resolves in
		// one katalog round-trip.
		unwatched := r.URL.Query().Get("unwatched") == "true" && userID != ""
		var (
			items []katalog.Item
			err   error
		)
		if unwatched {
			items, err = listUnwatched(r.Context(), st, userID, fetchPage, limit, offset)
		} else {
			items, err = fetchPage(limit, offset)
			if err == nil {
				stampWatchedSlice(r.Context(), st, userID, items)
			}
		}
		if err != nil {
			// 502: katalog is upstream of us and it is what failed. The client
			// can say "the catalog is unavailable" — which is true, actionable,
			// and cannot be mistaken for content.
			log.Printf("chino-api: katalog list failed (type=%q q=%q): %v", typ, q, err)
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"product": "chino",
				"error":   "catalog unavailable",
				"detail":  err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"product": "chino",
			"items":   items,
			"source":  "katalog",
		})
	}
}

func proxyArtwork(kc *katalog.Client, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		kc.ProxyStream(w, r, "/api/artwork/"+id+"/"+kind, bearerFrom(r))
	}
}

func proxyPlay(kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		kc.ProxyStream(w, r, "/api/play/"+id, bearerFrom(r))
	}
}

// proxyPlayInfo surfaces katalog-stream's transcode decision (codecs +
// reason) to the player UI.
func proxyPlayInfo(kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		kc.ProxyStream(w, r, "/api/play/"+id+"/info", bearerFrom(r))
	}
}

// proxyEmbeddedSubtitle forwards a <track src>-targeted request to
// katalog-stream's WebVTT extractor. The browser can't add auth headers
// to <track>, so the auth middleware accepts a ?token= query param and
// this proxy faithfully forwards the inbound query string.
func proxyEmbeddedSubtitle(kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		idx := chi.URLParam(r, "streamIndex")
		kc.ProxyStream(w, r, "/api/play/"+id+"/subtitles/"+idx+".vtt", bearerFrom(r))
	}
}

// subtitlesList returns the sidecar subtitle entries for an item with
// URLs the browser can mount as <track src>. katalog-api doesn't have
// a dedicated subtitles route; the data lives inside item-detail under
// include=subtitles. We fetch that, project to a flat list, and
// synthesise a URL per entry pointing at /api/v1/play/subs/{id}.vtt
// (proxied to chino-stream → file on the packages PVC).
//
// Empty list is a valid 200 — items genuinely have no sidecars all the
// time. Upstream errors bubble up as 502 so the player's error path
// shows something meaningful.
func subtitlesList(kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		it, err := kc.GetItemDetail(r.Context(), bearerFrom(r), id)
		if err != nil {
			http.Error(w, "katalog upstream: "+err.Error(), http.StatusBadGateway)
			return
		}
		if it == nil {
			http.Error(w, "item not found", http.StatusNotFound)
			return
		}
		subs := make([]katalog.Subtitle, 0, len(it.Subtitles))
		for _, s := range it.Subtitles {
			subs = append(subs, sidecarEntry(s))
		}
		writeJSON(w, http.StatusOK, map[string]any{"subtitles": subs})
	}
}

// sidecarEntry is a subtitle as the list gives it: with its URL, and with
// the format the URL serves. chino-stream serves a SubRip file at its .vtt
// URL as WebVTT, so a player, which picks its parser by the format, is told
// webvtt for it.
func sidecarEntry(s katalog.Subtitle) katalog.Subtitle {
	s.URL = "/api/v1/play/subs/" + s.ID + ".vtt"
	if strings.EqualFold(s.Format, "srt") {
		s.Format = "webvtt"
	}
	return s
}

// proxySidecarSubtitle forwards GET /api/v1/play/subs/{id}.vtt to
// chino-stream which reads the file from the packages PVC. Lives in
// the stream-token group so <track src> requests work without OIDC.
func proxySidecarSubtitle(kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		kc.ProxyStream(w, r, "/api/play/subs/"+id+".vtt", bearerFrom(r))
	}
}

// similarItems proxies "More like this" through to katalog-api, then
// stamps the per-user watched_at on each result so chino-web can mark
// already-seen items in the row without a follow-up fetch.
// Upstream 404 (unknown source id) propagates as 404; upstream errors
// surface as 502 so the UI hides the row instead of degrading silently.
func similarItems(st *store.Store, kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		limit := 12
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 50 {
				limit = n
			}
		}
		items, err := kc.ListSimilar(r.Context(), bearerFrom(r), id, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if items == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		userID, _ := auth.SubjectFromContext(r.Context())
		stampWatchedSlice(r.Context(), st, userID, items)
		writeJSON(w, http.StatusOK, map[string]any{
			"items": items,
			"total": len(items),
		})
	}
}

// itemDetail fetches a single item from katalog, with rich associations
// expanded (genres, cast, subtitles, trailers, extras, segments summary) so
// the detail page renders without follow-up fetches. The player page can
// safely call this too — the extra fields are small. trailers are the links
// to online videos installed clients know; the extras, each with the
// play_path of its master here, are beside them, never in them.
func itemDetail(st *store.Store, kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		item, err := kc.GetItemDetail(r.Context(), bearerFrom(r), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if item == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		userID, _ := auth.SubjectFromContext(r.Context())
		stampWatched(r.Context(), st, userID, []*katalog.Item{item})
		writeJSON(w, http.StatusOK, item)
	}
}

func bearerFrom(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}

// listExtensions serves the addon UI-extension contributions for a slot
// (?slot=search.empty), forwarding the caller's bearer to portal-api. Always
// 200 with an array — empty when no portal / no contributions.
func listExtensions(pc *portal.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slot := r.URL.Query().Get("slot")
		exts := pc.SlotExtensions(r.Context(), slot, bearerFrom(r))
		if exts == nil {
			exts = []portal.Extension{}
		}
		writeJSON(w, http.StatusOK, exts)
	}
}
