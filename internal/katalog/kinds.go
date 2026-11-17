package katalog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// A list chino-api keeps holds ids. What kind of title each one is (a movie,
// a series, an episode, …) is katalog-api's to say, by the item's type, and
// it says it one title at a time (GET /api/v1/items/{id}). Kinds asks it for
// the ids it has no answer of, a few at a time, and keeps each answer as it
// arrives. A title's kind is asked once at a time: a viewer who wants it while
// it is being asked waits for that answer.

// kindTTL is how long the kind of a title is kept: a title keeps its kind,
// so this is only how soon a title the catalog took in or dropped is seen.
const kindTTL = time.Hour

// kindNoneTTL is how long it is kept that katalog-api serves no title of an
// id: a title it takes in, or serves again after an import, goes where its
// kind says within it.
const kindNoneTTL = time.Minute

// kindRequests is how many requests for a title's kind are out at
// katalog-api at once, for every viewer together. Once katalog-api has
// failed as many in one call of Kinds, that call asks nothing more; the
// titles it failed for are asked after the others by the next.
const kindRequests = 8

// maxKindAsks is how many titles one call of Kinds asks katalog-api about:
// as many as one POST /me/available may ask about (maxAvailableIDs). The
// others are asked by a later call, once the ones before them are kept.
const maxKindAsks = 500

// kindWait is how long one call of Kinds waits for katalog-api, and
// kindAskWait how long one of its requests may take: a slow catalog holds a
// slot no longer than kindAskWait, and a viewer's answer no longer than
// kindWait.
const (
	kindWait    = 5 * time.Second
	kindAskWait = 2 * time.Second
)

// errKindsFailing is the answer of a title not asked because katalog-api had
// failed kindRequests times in the call.
var errKindsFailing = errors.New("katalog kinds: not asked, katalog-api failed for too many titles")

// kindAsk is a request for a title's kind out at katalog-api: done closes
// once its answer, kind or err, is in.
type kindAsk struct {
	done chan struct{}
	kind string
	err  error
}

// wait reports whether a's answer is in before ctx is done.
func (a *kindAsk) wait(ctx context.Context) bool {
	select {
	case <-a.done:
		return true
	default:
	}
	select {
	case <-a.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// Kinds is the kind katalog-api gives each of ids, by id: the item's type
// ("movie", "series", "episode", …), and "" for an id katalog-api serves the
// viewer no title of. The viewer's bearer and cap go with each request, as
// with every catalog request; each answer is kept as it arrives, per cap and
// id, for kindTTL ("" for kindNoneTTL), and at most kindRequests are asked at
// a time. An id whose kind is being asked for another call is not asked
// again: this call waits for that answer.
//
// An id whose kind katalog-api did not say in this call - it failed for it,
// did not answer within kindWait, or the id was past the first maxKindAsks
// asked - is left out of the answer, and no answer of it is kept; one it
// failed for is asked after the others for kindNoneTTL. The error is
// katalog-api's when it said the kind of none of ids, and the viewer's when
// the viewer went.
func (c *Client) Kinds(ctx context.Context, bearer string, ids []string) (map[string]string, error) {
	ids = onceEach(ids)
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	now := time.Now()
	deadline := now.Add(orDefault(c.kindWait, kindWait))
	key := capOf(ctx) + "|"
	waits := make(map[string]*kindAsk, len(ids))
	var mine, failedBefore []string
	var asks []*kindAsk
	register := func(id string) {
		if len(mine) < maxKindAsks {
			ask := &kindAsk{done: make(chan struct{})}
			c.visible.kindAsks[key+id] = ask
			waits[id] = ask
			mine, asks = append(mine, id), append(asks, ask)
		}
	}
	c.visible.mu.Lock()
	if c.visible.kind == nil {
		c.visible.kind = map[string]answer{}
	}
	if c.visible.kindAsks == nil {
		c.visible.kindAsks = map[string]*kindAsk{}
	}
	if c.visible.kindFailed == nil {
		c.visible.kindFailed = map[string]answer{}
	}
	for _, id := range ids {
		if a, ok := c.visible.kind[key+id]; ok && now.Before(a.until) {
			out[id] = a.item
		} else if ask, ok := c.visible.kindAsks[key+id]; ok {
			waits[id] = ask
		} else if f, ok := c.visible.kindFailed[key+id]; ok && now.Before(f.until) {
			failedBefore = append(failedBefore, id)
		} else {
			register(id)
		}
	}
	// A title katalog-api failed for within kindNoneTTL is asked after the
	// others, so that the titles it fails for do not use up the call's
	// kindRequests failures before the titles behind them are asked.
	for _, id := range failedBefore {
		register(id)
	}
	c.visible.mu.Unlock()

	if len(mine) > 0 {
		// This call's requests answer whoever waits for them: they are not
		// the viewer's alone, and end at the deadline, not when it goes.
		asking, stop := context.WithDeadline(context.WithoutCancel(ctx), deadline)
		go func() {
			defer stop()
			c.askKinds(asking, bearer, key, mine, asks)
		}()
	}

	wait, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var failed error
	for _, id := range ids {
		ask, ok := waits[id]
		if !ok || !ask.wait(wait) {
			continue
		}
		// A waiter takes the outcome of the ask it shares for this call,
		// the asker's deadline and a refusal of the asker's bearer
		// included. An error is never kept: the title goes with the shows
		// in this answer, and the next call asks again.
		if ask.err != nil {
			if failed == nil {
				failed = ask.err
			}
			continue
		}
		out[id] = ask.kind
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		if failed == nil {
			failed = wait.Err()
		}
		return nil, fmt.Errorf("katalog kinds: none of %d titles: %w", len(ids), failed)
	}
	return out, nil
}

// askKinds asks katalog-api the kind of each of ids, whose requests asks are,
// at most kindRequests at a time for every viewer together, each within
// kindAskWait, and answers each as it arrives (kindAnswered). The ones it
// does not ask - the wait is over, or katalog-api failed kindRequests times -
// it answers with why. It goes on when the viewer who asked goes: another may
// wait for its answers.
func (c *Client) askKinds(asking context.Context, bearer, key string, ids []string, asks []*kindAsk) {
	slots := c.kindSlotsOf()
	var (
		wg     sync.WaitGroup
		failed atomic.Int32
	)
	defer wg.Wait()
	for i, id := range ids {
		var why error
		select {
		case slots <- struct{}{}:
			switch {
			case asking.Err() != nil:
				why = asking.Err()
			case failed.Load() >= kindRequests:
				why = errKindsFailing
			}
			if why != nil {
				<-slots
			}
		case <-asking.Done():
			why = asking.Err()
		}
		if why != nil {
			for j := i; j < len(ids); j++ {
				c.kindAnswered(key, ids[j], asks[j], "", why, false)
			}
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			one, cancel := context.WithTimeout(asking, orDefault(c.kindAskWait, kindAskWait))
			defer cancel()
			it, err := c.GetItem(one, bearer, id)
			kind := ""
			if err != nil {
				failed.Add(1)
			} else if it != nil {
				kind = it.Type
			}
			c.kindAnswered(key, id, asks[i], kind, err, true)
		}()
	}
}

// kindAnswered is the answer of ask, the request for id's kind: kept unless
// it is an error, for kindTTL ("" for kindNoneTTL), and given to whoever
// waits for it. An error of katalog-api's (asked) is kept for kindNoneTTL
// as well, for the order in which Kinds asks (kindFailed), never as an
// answer.
func (c *Client) kindAnswered(key, id string, ask *kindAsk, kind string, err error, asked bool) {
	now := time.Now()
	c.visible.mu.Lock()
	delete(c.visible.kindAsks, key+id)
	if err == nil {
		ttl := kindTTL
		if kind == "" {
			ttl = kindNoneTTL
		}
		keep(c.visible.kind, key+id, answer{item: kind, until: now.Add(ttl)}, now)
		delete(c.visible.kindFailed, key+id)
	} else if asked {
		keep(c.visible.kindFailed, key+id, answer{until: now.Add(kindNoneTTL)}, now)
	}
	c.visible.mu.Unlock()
	ask.kind, ask.err = kind, err
	close(ask.done)
}

// kindSlotsOf is the client's slots for the requests of Kinds.
func (c *Client) kindSlotsOf() chan struct{} {
	c.kindOnce.Do(func() { c.kindSlots = make(chan struct{}, kindRequests) })
	return c.kindSlots
}

// orDefault is d, or def where d is 0.
func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
