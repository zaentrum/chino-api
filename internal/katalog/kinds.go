package katalog

import (
	"context"
	"sync"
	"time"
)

// A list chino-api keeps holds ids. What kind of title each one is (a movie,
// a series, an episode, …) is katalog-api's to say, by the item's type, and
// it says it one title at a time (GET /api/v1/items/{id}). Kinds asks it for
// the ids it has no answer of, a few at a time, and keeps each answer.

// kindTTL is how long the kind of a title is kept: a title keeps its kind,
// so this is only how soon a title the catalog took in or dropped is seen.
const kindTTL = time.Hour

// kindRequests is how many requests for a title's kind are out at
// katalog-api at once, for every viewer together.
const kindRequests = 8

// Kinds is the kind katalog-api gives each of ids, by id: the item's type
// ("movie", "series", "episode", …), and "" for an id katalog-api serves the
// viewer no title of. The viewer's bearer and cap go with each request, as
// with every catalog request; an answer is kept for kindTTL, per cap and id,
// and at most kindRequests are asked at a time. When katalog-api fails, the
// answer is the error, and what it answered before it is kept.
func (c *Client) Kinds(ctx context.Context, bearer string, ids []string) (map[string]string, error) {
	now := time.Now()
	key := capOf(ctx) + "|"
	out := make(map[string]string, len(ids))
	var ask []string
	c.visible.mu.Lock()
	if c.visible.kind == nil {
		c.visible.kind = map[string]answer{}
	}
	for _, id := range onceEach(ids) {
		if a, ok := c.visible.kind[key+id]; ok && now.Before(a.until) {
			out[id] = a.item
			continue
		}
		ask = append(ask, id)
	}
	c.visible.mu.Unlock()
	if len(ask) == 0 {
		return out, nil
	}

	asking, stop := context.WithCancel(ctx)
	defer stop()
	slots := c.kindSlotsOf()
	kinds := make([]string, len(ask))
	answered := make([]bool, len(ask))
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	// Once a request fails, or the viewer goes, nothing more is asked.
requests:
	for i, id := range ask {
		select {
		case <-asking.Done():
			break requests
		case slots <- struct{}{}:
		}
		if asking.Err() != nil {
			<-slots
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			it, err := c.GetItem(asking, bearer, id)
			if err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
				stop()
				return
			}
			if it != nil {
				kinds[i] = it.Type
			}
			answered[i] = true
		}()
	}
	wg.Wait()

	c.visible.mu.Lock()
	for i, id := range ask {
		if answered[i] {
			keep(c.visible.kind, key+id, answer{item: kinds[i], until: now.Add(kindTTL)}, now)
			out[id] = kinds[i]
		}
	}
	c.visible.mu.Unlock()
	if first != nil {
		return nil, first
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// kindSlotsOf is the client's slots for the requests of Kinds.
func (c *Client) kindSlotsOf() chan struct{} {
	c.kindOnce.Do(func() { c.kindSlots = make(chan struct{}, kindRequests) })
	return c.kindSlots
}
