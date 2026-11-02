package tracelog

import "time"

// nameTracker remembers when each file of a stream was collected, keyed by
// the part of its name before the first ".", so a later "Distributed file"
// line can be matched even when the output was renamed
// (file_x.el.gz → file_x.el.encoded / .mvne / .ict) or the line carries
// another file's ID.
//
// Entries live for ttl seconds (line time) and at most max entries are kept;
// the oldest go first. A collected file whose distribution is never logged
// (filtered, discarded, or logged while EventLink had stopped tracing) stays
// pending until it expires: trace-based pending is a hint, the buffer folders
// are the ground truth for files still waiting.
type nameTracker struct {
	ttl   float64
	max   int
	byKey map[string]*nameEntry
	fifo  []*nameEntry // insertion order, for expiry
	head  int

	evictedExpired map[string]uint64
	evictedLimit   map[string]uint64
}

type nameEntry struct {
	stream      string
	key         string
	collected   float64 // time of the earliest "Collected file" line; 0 = distribution seen first
	distributed float64 // time of the newest "Distributed file" line; 0 = none yet
	seen        float64 // line time the entry was created (expiry)
	pending     bool    // collected, not distributed yet
}

func newNameTracker(ttl time.Duration, max int) *nameTracker {
	return &nameTracker{
		ttl:            ttl.Seconds(),
		max:            max,
		byKey:          make(map[string]*nameEntry),
		evictedExpired: make(map[string]uint64),
		evictedLimit:   make(map[string]uint64),
	}
}

func nameKey(stream, base string) string { return stream + "\x00" + base }

func (n *nameTracker) add(e *nameEntry) {
	n.byKey[e.key] = e
	n.fifo = append(n.fifo, e)
	for len(n.byKey) > n.max && n.head < len(n.fifo) {
		n.evictFront(n.evictedLimit)
	}
}

// collected records a "Collected file" line.
func (n *nameTracker) collected(stream, base string, ts float64) {
	if base == "" {
		return
	}
	k := nameKey(stream, base)
	if e := n.byKey[k]; e != nil {
		switch {
		case e.pending:
			// Several nodes may log the collection (collector, then a
			// decoder's "Collected file -"): keep the earliest.
			if ts < e.collected {
				e.collected = ts
			}
			return
		case ts <= e.distributed:
			// Lines are out of order across rotated files: this collection
			// belongs to a file that is already distributed.
			if e.collected == 0 || ts < e.collected {
				e.collected = ts
			}
			return
		}
		// Otherwise a new file reuses the name.
	}
	n.add(&nameEntry{stream: stream, key: k, collected: ts, seen: ts, pending: true})
}

// distributed records a "Distributed file" line and returns the collected →
// distributed latency when the collection of that name is known.
func (n *nameTracker) distributed(stream, base string, ts float64) (float64, bool) {
	if base == "" {
		return 0, false
	}
	k := nameKey(stream, base)
	e := n.byKey[k]
	if e == nil {
		// Collected before the exporter started, or its line comes later
		// (out of order). Remember the distribution so a late "Collected
		// file" line does not leave a false pending file.
		n.add(&nameEntry{stream: stream, key: k, distributed: ts, seen: ts})
		return 0, false
	}
	if ts > e.distributed {
		e.distributed = ts
	}
	// Allow 1s for lines written in the same second in another order.
	if e.collected == 0 || ts < e.collected-1 {
		return 0, false
	}
	e.pending = false
	lat := ts - e.collected
	if lat < 0 {
		lat = 0
	}
	return lat, true
}

func (n *nameTracker) evictFront(reason map[string]uint64) {
	e := n.fifo[n.head]
	n.fifo[n.head] = nil
	n.head++
	if n.byKey[e.key] == e { // not replaced by a newer file with the same name
		delete(n.byKey, e.key)
		if e.pending {
			reason[e.stream]++
		}
	}
}

// expire drops entries older than ttl and compacts the queue.
func (n *nameTracker) expire(now float64) {
	cutoff := now - n.ttl
	for n.head < len(n.fifo) && n.fifo[n.head].seen < cutoff {
		n.evictFront(n.evictedExpired)
	}
	if n.head > 1024 && n.head > len(n.fifo)/2 {
		k := copy(n.fifo, n.fifo[n.head:])
		for i := k; i < len(n.fifo); i++ {
			n.fifo[i] = nil
		}
		n.fifo = n.fifo[:k]
		n.head = 0
	}
}

// pending summarises pending files per stream.
func (n *nameTracker) pending() map[string]PendingStats {
	out := make(map[string]PendingStats)
	for _, e := range n.byKey {
		if !e.pending {
			continue
		}
		p := out[e.stream]
		p.Files++
		if p.Oldest == 0 || e.collected < p.Oldest {
			p.Oldest = e.collected
		}
		out[e.stream] = p
	}
	for s, v := range n.evictedExpired {
		p := out[s]
		p.EvictedExpired = v
		out[s] = p
	}
	for s, v := range n.evictedLimit {
		p := out[s]
		p.EvictedLimit = v
		out[s] = p
	}
	return out
}
