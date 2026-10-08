package katalog

import (
	"context"
	"strconv"
	"time"

	"github.com/zaentrum/chino-api/internal/auth"
)

// One file can hold several episodes (a double-length finale listed as two),
// never split: katalog-api names the holder, the first episode the file
// holds, on each of the others (coveredBy), and the others on the holder
// (covers). The episodes of one file share what chino-api keeps of a
// viewer's playback - one position, one watched state - so a write for any
// of them is a write for each; FileEpisodes says which they are.

// fileTTL is how long the episodes of one file are kept: a player saves its
// position every few seconds and asks once in this time. An episode a scan
// links to a file, or unlinks from it, is written with the file within it.
const fileTTL = time.Minute

// FileEpisodes is the episodes of the one file the item id plays, as
// katalog-api names them: its holder first, then the episodes the holder
// covers, in episode order, id among them; id alone for an item with a file
// of its own, for one katalog-api does not know, and for every item of a
// katalog-api that names no covers. A covered episode's holder is asked for
// the others; a holder katalog-api does not serve the viewer is the file's
// all the same. Answers are kept for fileTTL, per cap and id; a failure is
// an error, kept for nothing.
func (c *Client) FileEpisodes(ctx context.Context, bearer, id string) ([]string, error) {
	now := time.Now()
	key := capOf(ctx) + "|" + id
	c.visible.mu.Lock()
	if c.visible.file == nil {
		c.visible.file = map[string]answer{}
	}
	a, ok := c.visible.file[key]
	c.visible.mu.Unlock()
	if ok && now.Before(a.until) {
		return append([]string(nil), a.items...), nil
	}
	ids, err := c.fileEpisodes(ctx, bearer, id)
	if err != nil {
		return nil, err
	}
	c.visible.mu.Lock()
	keep(c.visible.file, key, answer{items: ids, until: now.Add(fileTTL)}, now)
	c.visible.mu.Unlock()
	return append([]string(nil), ids...), nil
}

// fileEpisodes asks katalog-api for FileEpisodes: the item, and the holder
// of a covered one.
func (c *Client) fileEpisodes(ctx context.Context, bearer, id string) ([]string, error) {
	it, err := c.GetItem(ctx, bearer, id)
	if err != nil {
		return nil, err
	}
	if it == nil {
		return []string{id}, nil
	}
	holder, covers := id, it.Covers
	if it.CoveredBy != "" && it.CoveredBy != id {
		h, err := c.GetItem(ctx, bearer, it.CoveredBy)
		if err != nil {
			return nil, err
		}
		holder, covers = it.CoveredBy, nil
		if h != nil {
			covers = h.Covers
		}
	}
	return onceEach(append(append([]string{holder}, covers...), id)), nil
}

// onceEach is ids without the empty ones and without repeats, in their
// order.
func onceEach(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// capOf is the cap of ctx's viewer, as answers are kept by it: the age, or
// "-" for a viewer without a cap.
func capOf(ctx context.Context) string {
	if age, capped := auth.MaxRatingFromContext(ctx); capped {
		return strconv.Itoa(age)
	}
	return "-"
}
