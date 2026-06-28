package http

import (
	"net/http"

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
// items.
func proxyPackagedIDs(kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kc.ProxyStream(w, r, "/api/play/packaged-ids", bearerFrom(r))
	}
}

// proxyZapFeed forwards GET /api/v1/play/zap-feed to chino-stream's
// zap warm-pool endpoint. Used by the chino-web Zap pager as the
// primary source for its candidate queue; chino-stream maintains
// the pre-warmed pool in memory and the upstream call returns up to
// `limit` entries with per-item seekSec already baked in. The query
// string (limit=N) is preserved by ProxyStream.
func proxyZapFeed(kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kc.ProxyStream(w, r, "/api/play/zap-feed", bearerFrom(r))
	}
}

// proxyHLSQ forwards to /api/play/{id}/{quality}/<leaf> on
// katalog-stream. Used for per-quality playlists and init segments.
func proxyHLSQ(kc *katalog.Client, leaf string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		quality := chi.URLParam(r, "quality")
		kc.ProxyStream(w, r, "/api/play/"+id+"/"+quality+"/"+leaf, bearerFrom(r))
	}
}

// proxyHLSSegment forwards to /api/play/{id}/{quality}/{seg}.m4s on
// katalog-stream. The 6-second segment fetches are the hot path of
// the new pipeline — each runs short, so per-request transcode +
// io.Copy is fine.
func proxyHLSSegment(kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		quality := chi.URLParam(r, "quality")
		seg := chi.URLParam(r, "seg")
		kc.ProxyStream(w, r, "/api/play/"+id+"/"+quality+"/"+seg+".m4s", bearerFrom(r))
	}
}

// proxyHLSAudio forwards to /api/play/{id}/audio/{audioIdx}/<leaf> on
// katalog-stream — used for the per-audio-track media playlist and
// init segment.
func proxyHLSAudio(kc *katalog.Client, leaf string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		audioIdx := chi.URLParam(r, "audioIdx")
		kc.ProxyStream(w, r, "/api/play/"+id+"/audio/"+audioIdx+"/"+leaf, bearerFrom(r))
	}
}

// proxyHLSAudioSegment forwards to
// /api/play/{id}/audio/{audioIdx}/{seg}.m4s on katalog-stream.
func proxyHLSAudioSegment(kc *katalog.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		audioIdx := chi.URLParam(r, "audioIdx")
		seg := chi.URLParam(r, "seg")
		kc.ProxyStream(w, r, "/api/play/"+id+"/audio/"+audioIdx+"/"+seg+".m4s", bearerFrom(r))
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
