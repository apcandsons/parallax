// Package logbuf holds captured log lines: one ring buffer per process,
// global sequence numbers, and a merged view over all buffers. It has no
// knowledge of the TUI or of process management.
package logbuf

import (
	"sync"
	"time"
)

type Kind int

const (
	Stdout   Kind = iota
	Stderr        // rendered amber
	Event         // lifecycle info (start, restart) — rendered gray
	ErrEvent      // lifecycle failure (exit, kill) — rendered red
)

type Line struct {
	Seq  uint64
	Time time.Time
	Proc int // process index, in config order
	Kind Kind
	Text string
}

type Store struct {
	mu     sync.Mutex
	seq    uint64
	bufs   []*ring
	notify chan struct{}
}

func NewStore(numProcs, scrollback int) *Store {
	s := &Store{
		bufs:   make([]*ring, numProcs),
		notify: make(chan struct{}, 1),
	}
	for i := range s.bufs {
		s.bufs[i] = &ring{data: make([]Line, scrollback)}
	}
	return s
}

// Append stamps the line with the arrival time and the next global sequence
// number. Timestamps are assigned here, at read time, so the merged view
// reflects the order lines actually arrived in.
func (s *Store) Append(proc int, kind Kind, text string) {
	s.mu.Lock()
	s.seq++
	s.bufs[proc].append(Line{
		Seq:  s.seq,
		Time: time.Now(),
		Proc: proc,
		Kind: kind,
		Text: text,
	})
	s.mu.Unlock()

	select {
	case s.notify <- struct{}{}:
	default: // a notification is already pending; bursts coalesce
	}
}

// Notify signals after appends. Capacity 1: bursts collapse into a single
// pending notification, so a consumer that redraws per receive keeps up.
func (s *Store) Notify() <-chan struct{} { return s.notify }

// Proc returns a snapshot of one process's buffer, oldest first.
func (s *Store) Proc(i int) []Line {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bufs[i].snapshot()
}

// All merges every process's buffer by sequence number, oldest first.
func (s *Store) All() []Line {
	s.mu.Lock()
	snaps := make([][]Line, len(s.bufs))
	total := 0
	for i, b := range s.bufs {
		snaps[i] = b.snapshot()
		total += len(snaps[i])
	}
	s.mu.Unlock()

	out := make([]Line, 0, total)
	idx := make([]int, len(snaps))
	for {
		best := -1
		for j := range snaps {
			if idx[j] >= len(snaps[j]) {
				continue
			}
			if best == -1 || snaps[j][idx[j]].Seq < snaps[best][idx[best]].Seq {
				best = j
			}
		}
		if best == -1 {
			return out
		}
		out = append(out, snaps[best][idx[best]])
		idx[best]++
	}
}

type ring struct {
	data  []Line
	start int
	n     int
}

func (r *ring) append(l Line) {
	if r.n < len(r.data) {
		r.data[(r.start+r.n)%len(r.data)] = l
		r.n++
		return
	}
	r.data[r.start] = l
	r.start = (r.start + 1) % len(r.data)
}

func (r *ring) snapshot() []Line {
	out := make([]Line, r.n)
	for i := 0; i < r.n; i++ {
		out[i] = r.data[(r.start+i)%len(r.data)]
	}
	return out
}
