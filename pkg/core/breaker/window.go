package breaker

import "time"

// ratioWindow counts outcomes over a sliding Window using fixed slots:
// one slot per Window/ratioBuckets. It is O(1) per call and bounded in
// memory, unlike keeping every outcome.
type ratioWindow struct {
	slots    []slot
	span     time.Duration // the whole window
	width    time.Duration // one slot
	total    int
	failures int
}

const ratioBuckets = 10

// slot holds the outcomes inside [start, start+width).
type slot struct {
	start    time.Time
	total    int
	failures int
}

func newWindow(window time.Duration) *ratioWindow {
	width := window / ratioBuckets
	if width <= 0 {
		width = time.Nanosecond
	}
	return &ratioWindow{slots: make([]slot, ratioBuckets), span: width * ratioBuckets, width: width}
}

// add records one outcome observed at now.
func (w *ratioWindow) add(now time.Time, success bool) {
	s := w.advance(now)
	s.total++
	w.total++
	if !success {
		s.failures++
		w.failures++
	}
}

// advance returns the slot covering now. Each step computed from the clock
// expires every slot that fell out of the window, so a long quiet period
// empties the window regardless of how many steps went by — also when the
// clock was adjusted. Calls inside one width share a slot, so expired slots
// hold whole steps and a fresh call cannot land in one being expired.
func (w *ratioWindow) advance(now time.Time) *slot {
	for i := range w.slots {
		if s := &w.slots[i]; !s.start.IsZero() && now.Sub(s.start) >= w.span {
			w.total -= s.total
			w.failures -= s.failures
			*s = slot{}
		}
	}
	// start = floor(unixNano / width) * width, computed with bit shifts
	// instead of time.Truncate: Truncate counts days from year 1 and 1 is
	// not a whole number of slot widths, so it would not align to hours.
	width := uint64(w.width)
	start := time.Unix(0, int64(uint64(now.UnixNano())/width*width))
	if start.UnixNano() < 0 {
		start = start.Add(-w.width) // floor, not truncate, for times before 1970
	}
	idx := int((uint64(start.UnixNano()) / width) % uint64(len(w.slots)))
	s := &w.slots[idx]
	if !s.start.Equal(start) {
		// The position belonged to an older slot: its counts are stale (a
		// huge clock jump can reuse a position the step above did not visit).
		w.total -= s.total
		w.failures -= s.failures
		*s = slot{start: start}
	}
	return s
}

// tripped reports whether the window holds at least min outcomes and the
// failure ratio reaches ratio.
func (w *ratioWindow) tripped(ratio float64, min int) (bool, int) {
	if w.total < min {
		return false, w.total
	}
	return float64(w.failures)/float64(w.total) >= ratio, w.total
}

// clear forgets every outcome, for a fresh start after closing or opening.
func (w *ratioWindow) clear() {
	for i := range w.slots {
		w.slots[i] = slot{}
	}
	w.total, w.failures = 0, 0
}
