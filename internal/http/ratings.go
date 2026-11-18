package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/chino-api/internal/auth"
	"github.com/zaentrum/chino-api/internal/katalog"
)

// A capped viewer (auth.MaxRatingFromContext: its bearer's max_rating, or the
// cap of its stream token) is served only the titles its cap allows.
// katalog-api decides what that is: on every catalog request chino-api makes
// for the viewer (the katalog client passes the cap), and through
// /api/v1/visible for what chino-api serves or proxies without asking the
// catalog. A title the cap does not allow is as one there is not: every route
// of one title answers it 404, before anything goes upstream, so the answer
// does not say the title exists. An uncapped viewer is served as before.

// gate holds a capped viewer's requests for one title to what its cap
// allows.
type gate struct{ kc *katalog.Client }

// title is middleware for a route of one title, which the URL parameter
// param names: a capped viewer is answered 404 for a title its cap does not
// allow, and for an id that names none, as katalog-api answers both. When
// katalog-api cannot say, the answer is 502 and nothing is served.
func (g gate) title(param string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			age, capped := auth.MaxRatingFromContext(r.Context())
			if !capped {
				next.ServeHTTP(w, r)
				return
			}
			g.serve(w, r, next, age, chi.URLParam(r, param))
		})
	}
}

// subtitle is middleware for a route of one sidecar subtitle (the URL
// parameter id): a capped viewer is answered 404 for a subtitle of a title
// its cap does not allow, and for one there is not.
func (g gate) subtitle(next http.Handler) http.Handler {
	return g.owned(next, "subtitle", g.kc.SubtitleItem)
}

// font is middleware for a route of one font of a package (the URL parameter
// id), as subtitle is for a subtitle's: a capped viewer is answered 404 for a
// font of a title its cap does not allow, and for one there is not.
func (g gate) font(next http.Handler) http.Handler {
	return g.owned(next, "font", g.kc.FontItem)
}

// owned holds a capped viewer's request for one asset of a title (the URL
// parameter id) to what its cap allows, the asset's title as titleOf says
// it.
func (g gate) owned(next http.Handler, what string, titleOf func(context.Context, string) (string, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		age, capped := auth.MaxRatingFromContext(r.Context())
		if !capped {
			next.ServeHTTP(w, r)
			return
		}
		item, err := titleOf(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			logFailure(r, "katalog-api", "the title of a "+what, err)
			http.Error(w, errCatalog, http.StatusBadGateway)
			return
		}
		g.serve(w, r, next, age, item)
	})
}

// serve serves r with next when a viewer capped at age may be served the
// title id, and answers 404 otherwise.
func (g gate) serve(w http.ResponseWriter, r *http.Request, next http.Handler, age int, id string) {
	ok := false
	if id != "" {
		var err error
		if ok, err = g.kc.VisibleOne(r.Context(), age, id); err != nil {
			catalogUnavailable(w, r, err)
			return
		}
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	next.ServeHTTP(w, r)
}

// visibleIDs is ids without the titles the viewer's cap does not allow, and
// without the ids of no title, in their order: ids as they are for a viewer
// without a cap. The lists chino-api keeps itself (watchlists, likes) hold a
// title as the viewer saved it, whatever it is rated since, so a capped
// viewer's are filtered on the way out.
func (g gate) visibleIDs(ctx context.Context, ids []string) ([]string, error) {
	age, capped := auth.MaxRatingFromContext(ctx)
	if !capped || len(ids) == 0 {
		return ids, nil
	}
	return g.kc.Visible(ctx, age, ids)
}

// catalogUnavailable answers r, which could not be held to what the viewer
// may be served (its cap, or what the catalog holds): 502 errCatalog, and
// nothing of what it asked for; the failure is logged (logFailure).
func catalogUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	logFailure(r, "katalog-api", "what the viewer may be served", err)
	http.Error(w, errCatalog, http.StatusBadGateway)
}

// captured is an answer ProxyStream wrote, kept to be filtered before it
// goes out: at most maxHeld bytes of its body. A Write past them is refused
// (errOverHeld), and over is set.
type captured struct {
	header http.Header
	code   int
	body   bytes.Buffer
	over   bool
}

// maxHeld is the most of a body captured holds: far more than a list of
// chino-stream's comes to, and a bound on what one request keeps in memory.
const maxHeld = 8 << 20

// errOverHeld is an answer of chino-stream's longer than maxHeld, which
// filteredStream does not hold: 502 errPlayback, and logged as
// chino-stream's.
var errOverHeld = fmt.Errorf("chino-stream's answer is longer than the %d MiB held of it", maxHeld>>20)

func (c *captured) Header() http.Header { return c.header }
func (c *captured) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
	}
}
func (c *captured) Write(b []byte) (int, error) {
	c.WriteHeader(http.StatusOK)
	if c.over || c.body.Len()+len(b) > maxHeld {
		c.over = true
		return 0, errOverHeld
	}
	return c.body.Write(b)
}

// filteredStream serves a chino-stream list of titles (the Zap feed, the
// packaged ids) held to a capped viewer's cap: the answer is read whole, and
// filter takes out of its JSON what the cap does not allow. An uncapped
// viewer's answer is proxied as before; an answer that is no 200 goes out as
// ProxyStream wrote it, an upstream's error answer as a line of chino-api's
// own; a 200 that is not the list is 502 errPlayback (errNotAList), and so
// are one chino-stream broke off, of which nothing has gone out, and one
// longer than maxHeld, which is not held (errOverHeld).
func (g gate) filteredStream(kc *katalog.Client, path string,
	filter func(ctx context.Context, body []byte) ([]byte, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, capped := auth.MaxRatingFromContext(r.Context()); !capped {
			kc.ProxyStream(w, r, path, bearerFrom(r))
			return
		}
		c := &captured{header: http.Header{}}
		if !proxyWhole(kc, c, r, path) {
			http.Error(w, errPlayback, http.StatusBadGateway)
			return
		}
		if c.over {
			logError(r, "chino-stream failed", errOverHeld)
			http.Error(w, errPlayback, http.StatusBadGateway)
			return
		}
		body := c.body.Bytes()
		if c.code == http.StatusOK {
			var err error
			if body, err = filter(r.Context(), body); err != nil {
				if errors.Is(err, errNotAList) {
					// chino-stream's answer, not the catalog's.
					logError(r, "chino-stream failed", err)
					http.Error(w, errPlayback, http.StatusBadGateway)
					return
				}
				catalogUnavailable(w, r, err)
				return
			}
		}
		for k, vs := range c.header {
			if k != "Content-Length" {
				w.Header()[k] = vs
			}
		}
		w.WriteHeader(c.code)
		_, _ = w.Write(body)
	}
}

// proxyWhole is ProxyStream's answer to r of path, into c, and reports
// whether it is whole: false where the upstream broke it off after its first
// bytes, which ProxyStream logs and cuts off with http.ErrAbortHandler, here
// with nothing of it gone to the caller. Any other panic goes on.
func proxyWhole(kc *katalog.Client, c *captured, r *http.Request, path string) (whole bool) {
	defer func() {
		if v := recover(); v != nil {
			if v != http.ErrAbortHandler {
				panic(v)
			}
			whole = false
		}
	}()
	kc.ProxyStream(c, r, path, bearerFrom(r))
	return true
}

// errNotAList is an answer of chino-stream's, of 200, that is not the list it
// was asked for (filteredStream): 502 errPlayback, and logged as
// chino-stream's.
var errNotAList = errors.New("chino-stream's answer is not its list")

// errPlayback is the answer, with 502, of a route whose chino-stream did not
// answer as it does.
const errPlayback = "playback unavailable"

// zapFeed takes out of chino-stream's Zap feed ({"items": [{"id": ...}, ...],
// ...}) the cards whose titles the viewer's cap does not allow, keeping
// everything else of it as it is.
func (g gate) zapFeed(ctx context.Context, body []byte) ([]byte, error) {
	var feed map[string]json.RawMessage
	if err := json.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("%w: zap feed: %w", errNotAList, err)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(feed["items"], &items); err != nil && feed["items"] != nil {
		return nil, fmt.Errorf("%w: zap feed items: %w", errNotAList, err)
	}
	ids := make([]string, len(items))
	for i, it := range items {
		var card struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(it, &card)
		ids[i] = card.ID
	}
	allowed, err := g.visibleIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	keep := make(map[string]bool, len(allowed))
	for _, id := range allowed {
		keep[id] = true
	}
	kept := make([]json.RawMessage, 0, len(items))
	for i, it := range items {
		if keep[ids[i]] {
			kept = append(kept, it)
		}
	}
	if feed["items"], err = json.Marshal(kept); err != nil {
		return nil, err
	}
	return json.Marshal(feed)
}

// packagedIDs takes out of chino-stream's packaged ids ({"ids": [...]}) the
// titles the viewer's cap does not allow.
func (g gate) packagedIDs(ctx context.Context, body []byte) ([]byte, error) {
	var list map[string]json.RawMessage
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("%w: packaged ids: %w", errNotAList, err)
	}
	var ids []string
	if err := json.Unmarshal(list["ids"], &ids); err != nil && list["ids"] != nil {
		return nil, fmt.Errorf("%w: packaged ids: %w", errNotAList, err)
	}
	allowed, err := g.visibleIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	if allowed == nil {
		allowed = []string{}
	}
	if list["ids"], err = json.Marshal(allowed); err != nil {
		return nil, err
	}
	return json.Marshal(list)
}
