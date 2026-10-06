package http

import (
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/katalog"
)

// proxyHLS forwards to /api/play/{id}/<leaf> on katalog-stream.
// Used for the master playlist where the URL has no quality segment.
func proxyHLS(kc *katalog.Client, leaf string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		kc.ProxyStream(w, r, "/api/play/"+id+"/"+leaf, bearerFrom(r))
	}
}

// proxyPackagedIDs forwards GET /api/v1/play/packaged-ids to
// /api/play/packaged-ids on katalog-stream. No item-id substitution,
// just a static path forward — used by the Zap pager once per
// session to filter its candidate pool to instant-start (packaged)
// items. A capped viewer's list leaves out the titles its cap does not
// allow (gate.packagedIDs).
func proxyPackagedIDs(kc *katalog.Client, g gate) http.HandlerFunc {
	return g.filteredStream(kc, "/api/play/packaged-ids", g.packagedIDs)
}

// proxyZapFeed forwards GET /api/v1/play/zap-feed to chino-stream's
// zap warm-pool endpoint. Used by the chino-web Zap pager as the
// primary source for its candidate queue; chino-stream maintains
// the pre-warmed pool in memory and the upstream call returns up to
// `limit` entries with per-item seekSec already baked in. The query
// string (limit=N) is preserved by ProxyStream. A capped viewer's feed
// leaves out the cards whose titles its cap does not allow (gate.zapFeed).
func proxyZapFeed(kc *katalog.Client, g gate) http.HandlerFunc {
	return g.filteredStream(kc, "/api/play/zap-feed", g.zapFeed)
}

// playUpstream is chino-stream's path of what a play request names, the
// on-the-fly ladder's routes under it: an item's (itemUpstream) or an
// extra's (extraUpstream). "" when it has answered the request itself, and
// chino-stream is not asked.
type playUpstream func(w http.ResponseWriter, r *http.Request) string

// itemUpstream is chino-stream's path of the item r names, /api/play/{id}.
func itemUpstream(_ http.ResponseWriter, r *http.Request) string {
	return "/api/play/" + chi.URLParam(r, "id")
}

// proxyHLSQ forwards an on-the-fly rung's media playlist or init segment,
// .../play/{quality}/<leaf> of an item or an extra, to chino-stream's
// <upstream>/{quality}/<leaf>.
func proxyHLSQ(kc *katalog.Client, upstream playUpstream, leaf string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if base := upstream(w, r); base != "" {
			kc.ProxyStream(w, r, base+"/"+chi.URLParam(r, "quality")+"/"+leaf, bearerFrom(r))
		}
	}
}

// proxyHLSSegment forwards one on-the-fly media segment,
// .../play/{quality}/{seg}.m4s of an item or an extra, to chino-stream's
// <upstream>/{quality}/{seg}.m4s: the hot path of the live pipeline.
// chino-stream serves a segment as ffmpeg finishes it, so one may take a
// moment to start; ProxyStream's stream client sets no deadline, the play
// group no middleware.Timeout, and a client gone cancels the request.
func proxyHLSSegment(kc *katalog.Client, upstream playUpstream) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if base := upstream(w, r); base != "" {
			kc.ProxyStream(w, r, base+"/"+chi.URLParam(r, "quality")+"/"+chi.URLParam(r, "seg")+".m4s", bearerFrom(r))
		}
	}
}

// proxyHLSAudio forwards an on-the-fly audio track's media playlist or init
// segment, .../play/audio/{audioIdx}/<leaf> of an item or an extra, to
// chino-stream's <upstream>/audio/{audioIdx}/<leaf>.
func proxyHLSAudio(kc *katalog.Client, upstream playUpstream, leaf string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if base := upstream(w, r); base != "" {
			kc.ProxyStream(w, r, base+"/audio/"+chi.URLParam(r, "audioIdx")+"/"+leaf, bearerFrom(r))
		}
	}
}

// proxyHLSAudioSegment forwards one on-the-fly audio segment,
// .../play/audio/{audioIdx}/{seg}.m4s of an item or an extra, to
// chino-stream's <upstream>/audio/{audioIdx}/{seg}.m4s, as proxyHLSSegment
// does a rung's.
func proxyHLSAudioSegment(kc *katalog.Client, upstream playUpstream) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if base := upstream(w, r); base != "" {
			kc.ProxyStream(w, r, base+"/audio/"+chi.URLParam(r, "audioIdx")+"/"+chi.URLParam(r, "seg")+".m4s", bearerFrom(r))
		}
	}
}

// proxyPackagedRendition forwards to /api/play/{id}/{rendId}/<leaf>
// for packaged-CMAF playlists, init, and iframe playlists. RendIds
// are vN/aN, scoped by the route regex in router.go.
func proxyPackagedRendition(kc *katalog.Client, leaf string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		rendID := chi.URLParam(r, "rendId")
		kc.ProxyStream(w, r, "/api/play/"+id+"/"+rendID+"/"+leaf, bearerFrom(r))
	}
}

// proxyPackagedSegment forwards to
// /api/play/{id}/{rendId}/seg-{seg}.m4s — packaged CMAF segments.
// Same shape as proxyHLSSegment but with the rendition naming
// scheme shaka uses (seg-NNNNN.m4s, not just NN.m4s).
func proxyPackagedSegment(kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		rendID := chi.URLParam(r, "rendId")
		seg := chi.URLParam(r, "seg")
		kc.ProxyStream(w, r, "/api/play/"+id+"/"+rendID+"/seg-"+seg+".m4s", bearerFrom(r))
	}
}

// proxyPackagedSubtitleSegment forwards to
// /api/play/{id}/{rendId}/seg-{seg}.vtt — one WebVTT segment of a
// packaged HLS subtitle rendition (sN, scoped by the route regex in
// router.go). Its media playlist goes through proxyPackagedRendition.
func proxyPackagedSubtitleSegment(kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		rendID := chi.URLParam(r, "rendId")
		seg := chi.URLParam(r, "seg")
		kc.ProxyStream(w, r, "/api/play/"+id+"/"+rendID+"/seg-"+seg+".vtt", bearerFrom(r))
	}
}

// proxyTrickplayVTT forwards to /api/play/{id}/trickplay/thumbnails.vtt
// — the WebVTT cue file that maps scrub timestamps to sprite-sheet
// coordinates. Written once per item by the analyzer; served as a
// static file from /media/packages/.../trickplay/.
func proxyTrickplayVTT(kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		kc.ProxyStream(w, r, "/api/play/"+id+"/trickplay/thumbnails.vtt", bearerFrom(r))
	}
}

// proxyTrickplaySprite forwards to
// /api/play/{id}/trickplay/sprite-{n}.jpg — one tile sheet of
// thumbnails. The {n} regex in router.go pins it to digits; we just
// substitute back into the upstream path.
func proxyTrickplaySprite(kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		n := chi.URLParam(r, "n")
		kc.ProxyStream(w, r, "/api/play/"+id+"/trickplay/sprite-"+n+".jpg", bearerFrom(r))
	}
}

// extraID is what an extra's id may be on the way to chino-stream: a UUID,
// as katalog-manager gives them, so nothing that reads as more than one path
// segment is forwarded. The parameter is extraId; {id} is the title's, which
// the parental gate checks.
var extraID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// extraUpstream is chino-stream's path of the extra r names,
// /api/play/{id}/extras/{extraId}, or "" when the extra's id is no UUID,
// which it has answered 404 without asking chino-stream.
func extraUpstream(w http.ResponseWriter, r *http.Request) string {
	extra := chi.URLParam(r, "extraId")
	if !extraID.MatchString(extra) {
		http.Error(w, "not found", http.StatusNotFound)
		return ""
	}
	return "/api/play/" + chi.URLParam(r, "id") + "/extras/" + extra
}

// proxyExtra forwards /api/v1/items/{id}/extras/{extraId}/play/<leaf> (the
// master) to chino-stream's /api/play/{id}/extras/{extraId}/<leaf>.
func proxyExtra(kc *katalog.Client, leaf string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if base := extraUpstream(w, r); base != "" {
			kc.ProxyStream(w, r, base+"/"+leaf, bearerFrom(r))
		}
	}
}

// proxyExtraRendition forwards a rendition's media playlist, I-frame
// playlist or init segment of an extra: .../play/{rendId}/<leaf> to
// chino-stream's /api/play/{id}/extras/{extraId}/{rendId}/<leaf>.
func proxyExtraRendition(kc *katalog.Client, leaf string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if base := extraUpstream(w, r); base != "" {
			kc.ProxyStream(w, r, base+"/"+chi.URLParam(r, "rendId")+"/"+leaf, bearerFrom(r))
		}
	}
}

// proxyExtraSegment forwards one segment of an extra's rendition,
// .../play/{rendId}/seg-{seg}<ext> (.m4s, or .vtt of an sN rendition), to
// chino-stream's /api/play/{id}/extras/{extraId}/{rendId}/seg-{seg}<ext>.
func proxyExtraSegment(kc *katalog.Client, ext string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if base := extraUpstream(w, r); base != "" {
			kc.ProxyStream(w, r, base+"/"+chi.URLParam(r, "rendId")+"/seg-"+chi.URLParam(r, "seg")+ext, bearerFrom(r))
		}
	}
}
