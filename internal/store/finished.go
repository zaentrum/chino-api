package store

import "fmt"

// When a title is finished: the one rule, for films and episodes alike, of
// every place chino-api serves a resume point or the watched state. A title
// whose saved position is in its last 5 %, or its last 2 minutes where that
// is longer, is finished: it counts as watched (postProgress marks it, but
// for a dismiss, which only takes it off Continue Watching), and it has no
// resume point - GET progress says 0 (and finished), Continue Watching drops
// a film and puts an episode's next in its place, with that one's own resume
// point or none. The clients then show Play from the start. The end is never
// more than the title's second half: a short of 90 s is not finished at its
// first second. A title whose length is not known is never finished by its
// position.
//
// finishedSQL is the same rule on a playback_progress row named p, for the
// queries that decide it in the database.

const (
	// finishedTailPercent is the share of a title, at its end, a position
	// in which finishes it.
	finishedTailPercent = 5
	// finishedTailMinSec is the end a position in which finishes a title,
	// at the least: a short one's last 5 % would be only its credits' start.
	finishedTailMinSec = 120
)

// MinResumeSec is how far into a title a position must be for Continue
// Watching to show it as one to resume: the first 30 s are a start.
const MinResumeSec = 30

// Finished reports whether positionSec, in a title durationSec long, is in
// its finished end (see above). False where the length is not known.
func Finished(positionSec, durationSec int) bool {
	if durationSec <= 0 {
		return false
	}
	return positionSec >= durationSec-finishedTail(durationSec)
}

// finishedTail is how much of a title, at its end, finishes it: its last
// 5 %, at least its last 2 minutes, at most its second half. In whole
// seconds, the way the database does it (integer division).
func finishedTail(durationSec int) int {
	tail := durationSec * finishedTailPercent / 100
	if tail < finishedTailMinSec {
		tail = finishedTailMinSec
	}
	if half := durationSec / 2; tail > half {
		tail = half
	}
	return tail
}

// finishedSQL is Finished on the playback_progress row p, of the same
// constants.
var finishedSQL = fmt.Sprintf(
	`(p.duration_sec > 0 AND p.position_sec >= p.duration_sec - LEAST(GREATEST(p.duration_sec * %d / 100, %d), p.duration_sec / 2))`,
	finishedTailPercent, finishedTailMinSec,
)
