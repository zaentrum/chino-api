package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		age, capped := auth.MaxRatingFromContext(r.Context())
		if !capped {
			next.ServeHTTP(w, r)
			return
		}
		item, err := g.kc.SubtitleItem(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			log.Printf("chino-api: the title of a subtitle could not be looked up: %v", err)
			http.Error(w, "catalog unavailable", http.StatusBadGateway)
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
			log.Printf("chino-api: what a rating cap allows could not be asked: %v", err)
			http.Error(w, "catalog unavailable", http.StatusBadGateway)
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

// catalogUnavailable answers a list that could not be held to the viewer's
// cap: 502, and nothing of the list.
func catalogUnavailable(w http.ResponseWriter, err error) {
	log.Printf("chino-api: what a rating cap allows could not be asked: %v", err)
	http.Error(w, "catalog unavailable", http.StatusBadGateway)
}

// captured is an answer ProxyStream wrote, kept to be filtered before it
// goes out.
type captured struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (c *captured) Header() http.Header { return c.header }
func (c *captured) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
	}
}
func (c *captured) Write(b []byte) (int, error) {
	c.WriteHeader(http.StatusOK)
	return c.body.Write(b)
}

// filteredStream serves a chino-stream list of titles (the Zap feed, the
// packaged ids) held to a capped viewer's cap: the answer is read whole, and
// filter takes out of its JSON what the cap does not allow. An uncapped
// viewer's answer is proxied as before; an answer that is no 200 goes out as
// it came.
func (g gate) filteredStream(kc *katalog.Client, path string,
	filter func(ctx context.Context, body []byte) ([]byte, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, capped := auth.MaxRatingFromContext(r.Context()); !capped {
			kc.ProxyStream(w, r, path, bearerFrom(r))
			return
		}
		c := &captured{header: http.Header{}}
		kc.ProxyStream(c, r, path, bearerFrom(r))
		body := c.body.Bytes()
		if c.code == http.StatusOK {
			var err error
			if body, err = filter(r.Context(), body); err != nil {
				catalogUnavailable(w, err)
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

// zapFeed takes out of chino-stream's Zap feed ({"items": [{"id": ...}, ...],
// ...}) the cards whose titles the viewer's cap does not allow, keeping
// everything else of it as it is.
func (g gate) zapFeed(ctx context.Context, body []byte) ([]byte, error) {
	var feed map[string]json.RawMessage
	if err := json.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("zap feed: %w", err)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(feed["items"], &items); err != nil && feed["items"] != nil {
		return nil, fmt.Errorf("zap feed items: %w", err)
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
		return nil, fmt.Errorf("packaged ids: %w", err)
	}
	var ids []string
	if err := json.Unmarshal(list["ids"], &ids); err != nil && list["ids"] != nil {
		return nil, fmt.Errorf("packaged ids: %w", err)
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
