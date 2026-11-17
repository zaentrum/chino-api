package katalog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// A list chino-api keeps holds ids. What kind of title each one is (a movie,
// a series, an episode, …) is katalog-api's to say, by the item's type, and
// it says it one title at a time (GET /api/v1/items/{id}). Kinds asks it for
// the ids it has no answer of, a few at a time, and keeps each answer as it
// arrives. A title's kind is asked once at a time: a viewer who wants it while
// it is being asked waits for that answer, and asks it once itself where that
// request ended in an error and its own wait has time left.

// kindTTL is how long the kind of a title is kept: a title keeps its kind,
// so this is only how soon a title the catalog took in or dropped is seen.
const kindTTL = time.Hour

// kindNoneTTL is how long it is kept that katalog-api serves no title of an
// id: a title it takes in, or serves again after an import, goes where its
// kind says within it.
const kindNoneTTL = time.Minute

// kindFailedTTL is how long the mark of a title katalog-api failed to say the
// kind of is kept (kindFailed): a day, longer than the time between one visit
// to the lists and the next of a person who opens them every day, so that the
// titles katalog-api keeps failing for stay behind the others from one visit
// to the next, and the first request of a visit says the titles behind them.
// A mark only orders the asking and never answers for a title; it goes once
// katalog-api says the title's kind, and the marks kept are bounded as every
// answer is (maxAnswers).
const kindFailedTTL = 24 * time.Hour

// kindRequests is how many requests for a title's kind are out at
// katalog-api at once, for every viewer together. Once katalog-api has
// failed as many in one call of Kinds, that call asks nothing more; the
// titles it failed for are asked after the others by a later call.
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

// kindCall is one call of Kinds: the viewer's bearer and its cap (key, as
// answers are kept), until when it waits, how many titles it has asked
// katalog-api itself, and how often katalog-api has failed it.
type kindCall struct {
	bearer, key string
	deadline    time.Time
	asked       int
	failed      atomic.Int32
}

// kindAsk is a request for a title's kind out at katalog-api, for call: done
// closes once its answer, kind or err, is in.
type kindAsk struct {
	call *kindCall
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
// again: this call waits for that answer. Where that request ended in an
// error (the other call's deadline, a refusal of its bearer, a title it no
// longer asked after kindRequests failures) and this call's wait has time
// left, this call asks the id once itself, with its own bearer, within its
// own deadline, maxKindAsks and kindRequests failures; or, where another
// call asks it again by then, waits for that answer.
//
// An id whose kind katalog-api did not say in this call - it failed for it,
// did not answer within kindWait, or the id was past the first maxKindAsks
// asked - is left out of the answer, and no answer of it is kept; one it
// failed for is asked after the others for kindFailedTTL, the one it failed
// for longest ago first. A viewer gone before the call asks anything asks
// katalog-api nothing. The error is katalog-api's when it said the kind of
// none of ids, and the viewer's when the viewer went.
func (c *Client) Kinds(ctx context.Context, bearer string, ids []string) (map[string]string, error) {
	ids = onceEach(ids)
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	// Gone before it registers anything: nobody waits for what it would ask.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	call := &kindCall{bearer: bearer, key: capOf(ctx) + "|",
		deadline: time.Now().Add(orDefault(c.kindWait, kindWait))}
	wait, cancel := context.WithDeadline(ctx, call.deadline)
	defer cancel()
	again, failed := c.kindsOf(ctx, wait, call, ids, out)
	if len(again) > 0 && wait.Err() == nil {
		// The ids whose request, another call's, ended in an error: asked
		// once more, by this call.
		if _, err := c.kindsOf(ctx, wait, call, again, out); err != nil {
			failed = err
		}
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

// kindsOf puts the kind of each of ids into out, for call: as kept; else the
// answer of the request out for it; else that of a request call registers and
// asks itself (askKinds), the titles katalog-api failed for after the others.
// It waits for the answers within wait, and answers the ids whose request,
// another call's, ended in an error, and the first error of one.
func (c *Client) kindsOf(ctx, wait context.Context, call *kindCall, ids []string, out map[string]string) (again []string, failed error) {
	now := c.clock()
	waits := make(map[string]*kindAsk, len(ids))
	var mine, failedBefore []string
	var asks []*kindAsk
	register := func(id string) {
		if call.asked < maxKindAsks {
			ask := &kindAsk{call: call, done: make(chan struct{})}
			c.visible.kindAsks[call.key+id] = ask
			waits[id] = ask
			mine, asks = append(mine, id), append(asks, ask)
			call.asked++
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
		if a, ok := c.visible.kind[call.key+id]; ok && now.Before(a.until) {
			out[id] = a.item
		} else if ask, ok := c.visible.kindAsks[call.key+id]; ok {
			waits[id] = ask
		} else if f, ok := c.visible.kindFailed[call.key+id]; ok && now.Before(f.until) {
			failedBefore = append(failedBefore, id)
		} else {
			register(id)
		}
	}
	// A title katalog-api failed for within kindFailedTTL is asked after the
	// others, so that the titles it fails for do not use up the call's
	// kindRequests failures before the titles behind them are asked; the one
	// it failed for longest ago first, so that one which has healed comes to
	// the front of them.
	slices.SortStableFunc(failedBefore, func(a, b string) int {
		return c.visible.kindFailed[call.key+a].until.Compare(c.visible.kindFailed[call.key+b].until)
	})
	for _, id := range failedBefore {
		register(id)
	}
	c.visible.mu.Unlock()
	if c.kindHook != nil {
		c.kindHook()
	}

	if len(mine) > 0 {
		// This call's requests answer whoever waits for them: they are not
		// the viewer's alone, and end at the deadline, not when it goes.
		asking, stop := context.WithDeadline(context.WithoutCancel(ctx), call.deadline)
		go func() {
			defer stop()
			c.askKinds(asking, call, mine, asks)
		}()
	}

	for _, id := range ids {
		ask, ok := waits[id]
		if !ok || !ask.wait(wait) {
			continue
		}
		// An error is kept only as the mark of the order in which titles are
		// asked (kindFailed), never as an answer: the title goes with the
		// shows in this answer, and a later call asks again. Where the
		// request was another call's, this one may ask it itself (Kinds).
		if ask.err != nil {
			if failed == nil {
				failed = ask.err
			}
			if ask.call != call {
				again = append(again, id)
			}
			continue
		}
		out[id] = ask.kind
	}
	return again, failed
}

// askKinds asks katalog-api the kind of each of ids for call, whose requests
// asks are, at most kindRequests at a time for every viewer together, each
// within kindAskWait, and answers each as it arrives (kindAnswered). The ones
// it does not ask - the wait is over, or katalog-api failed call kindRequests
// times - it answers with why. It goes on when the viewer who asked goes:
// another may wait for its answers.
func (c *Client) askKinds(asking context.Context, call *kindCall, ids []string, asks []*kindAsk) {
	slots := c.kindSlotsOf()
	var wg sync.WaitGroup
	defer wg.Wait()
	for i, id := range ids {
		var why error
		select {
		case slots <- struct{}{}:
			switch {
			case asking.Err() != nil:
				why = asking.Err()
			case call.failed.Load() >= kindRequests:
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
				c.kindAnswered(call.key, ids[j], asks[j], "", why, false)
			}
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			one, cancel := context.WithTimeout(asking, orDefault(c.kindAskWait, kindAskWait))
			defer cancel()
			it, err := c.GetItem(one, call.bearer, id)
			kind := ""
			if err != nil {
				call.failed.Add(1)
			} else if it != nil {
				kind = it.Type
			}
			c.kindAnswered(call.key, id, asks[i], kind, err, true)
		}()
	}
}

// kindAnswered is the answer of ask, the request for id's kind: kept unless
// it is an error, for kindTTL ("" for kindNoneTTL), and given to whoever
// waits for it. An error of katalog-api's (asked) is kept for kindFailedTTL
// as the mark of the order in which Kinds asks (kindFailed), never as an
// answer.
func (c *Client) kindAnswered(key, id string, ask *kindAsk, kind string, err error, asked bool) {
	now := c.clock()
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
		keep(c.visible.kindFailed, key+id, answer{until: now.Add(kindFailedTTL)}, now)
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

// clock is the time the answers of Kinds are kept by: c.now where it is set,
// else the time.
func (c *Client) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// orDefault is d, or def where d is 0.
func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
