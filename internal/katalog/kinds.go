package katalog

import (
	"context"
	"sync"
	"time"
)

// A list chino-api keeps holds ids alone, and katalog-api says a title's kind
// only on its item route, so sorting a list by kind asks once per title. A
// title does not change kind, so the answers are kept long and the first look
// at a list is the only costly one.

// kindTTL is how long a title's kind is kept.
const kindTTL = 24 * time.Hour

// kindLookups is how many titles Kinds asks about at a time.
const kindLookups = 8

// Kinds is the kind of each of ids that names a title the caller may see, by
// id. An id of no title is left out. One katalog-api does not answer for fails
// the whole answer, so a title is never sorted or counted for want of one.
func (c *Client) Kinds(ctx context.Context, bearer string, ids []string) (map[string]string, error) {
	now := time.Now()
	out := make(map[string]string, len(ids))
	seen := make(map[string]bool, len(ids))
	var ask []string
	c.visible.mu.Lock()
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if a, ok := c.visible.kind[id]; ok && now.Before(a.until) {
			out[id] = a.item
			continue
		}
		ask = append(ask, id)
	}
	c.visible.mu.Unlock()
	if len(ask) == 0 {
		return out, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	next := make(chan string)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	for range min(kindLookups, len(ask)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range next {
				it, err := c.GetItem(ctx, bearer, id)
				mu.Lock()
				switch {
				case err != nil:
					if first == nil {
						first = err
						cancel()
					}
				case it != nil:
					out[id] = it.Type
				}
				mu.Unlock()
			}
		}()
	}
send:
	for _, id := range ask {
		select {
		case next <- id:
		case <-ctx.Done():
			break send
		}
	}
	close(next)
	wg.Wait()
	if first != nil {
		return nil, first
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.visible.mu.Lock()
	defer c.visible.mu.Unlock()
	if c.visible.kind == nil {
		c.visible.kind = map[string]answer{}
	}
	for _, id := range ask {
		if kind, ok := out[id]; ok {
			keep(c.visible.kind, id, answer{item: kind, until: now.Add(kindTTL)}, now)
		}
	}
	return out, nil
}
