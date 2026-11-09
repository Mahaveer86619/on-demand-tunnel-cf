package tunnel

import (
	"sync"
	"sync/atomic"
	"time"
)

type Stats struct {
	since          time.Time
	requests       atomic.Uint64
	bytesOut       atomic.Uint64
	byClass        [6]atomic.Uint64
	upstreamErrors atomic.Uint64
	active         atomic.Int64
	lastRequest    atomic.Int64
}

type StatsSnapshot struct {
	Since          time.Time         `json:"since"`
	Requests       uint64            `json:"requests"`
	BytesOut       uint64            `json:"bytes_out"`
	Status         map[string]uint64 `json:"status"`
	UpstreamErrors uint64            `json:"upstream_errors"`
	Active         int64             `json:"active"`
	LastRequest    *time.Time        `json:"last_request,omitempty"`
}

func newStats() *Stats { return &Stats{since: time.Now().UTC()} }

func (s *Stats) observe(status int, bytes int64, at time.Time) {
	s.requests.Add(1)
	s.bytesOut.Add(uint64(bytes))
	if c := status / 100; c >= 1 && c <= 5 {
		s.byClass[c].Add(1)
	}
	s.lastRequest.Store(at.UnixNano())
}

func (s *Stats) snapshot() StatsSnapshot {
	snap := StatsSnapshot{
		Since:          s.since,
		Requests:       s.requests.Load(),
		BytesOut:       s.bytesOut.Load(),
		Status:         make(map[string]uint64, 5),
		UpstreamErrors: s.upstreamErrors.Load(),
		Active:         s.active.Load(),
	}
	for c := 1; c <= 5; c++ {
		if n := s.byClass[c].Load(); n > 0 {
			snap.Status[string(rune('0'+c))+"xx"] = n
		}
	}
	if ns := s.lastRequest.Load(); ns > 0 {
		t := time.Unix(0, ns).UTC()
		snap.LastRequest = &t
	}
	return snap
}

// ring keeps the most recent n values.
type ring[T any] struct {
	mu   sync.Mutex
	buf  []T
	next int
	full bool
}

func newRing[T any](n int) *ring[T] { return &ring[T]{buf: make([]T, n)} }

func (r *ring[T]) add(v T) {
	r.mu.Lock()
	r.buf[r.next] = v
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// last returns up to n values, newest first.
func (r *ring[T]) last(n int) []T {
	r.mu.Lock()
	defer r.mu.Unlock()
	size := r.next
	if r.full {
		size = len(r.buf)
	}
	if n <= 0 || n > size {
		n = size
	}
	out := make([]T, n)
	for i := range out {
		out[i] = r.buf[(r.next-1-i+len(r.buf))%len(r.buf)]
	}
	return out
}
