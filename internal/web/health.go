package web

import (
	"context"
	"sync"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/store"
)

// healthCacheTTL is how long a /healthz aggregate is reused.
//
// /healthz is deliberately unauthenticated so a container healthcheck does not
// need the token in its command, its compose file and every uptime monitor's
// config. The cost of that decision is that anyone who can reach the port can
// ask for an answer as often as they like, and the answer was a full aggregate
// over the call table — several SUMs and COUNTs across every row stored. On a
// network-exposed instance that is a CPU-exhaustion primitive with no rate limit
// in front of it, handed out for free by the same design choice that keeps the
// healthcheck working.
//
// Five seconds is far below the granularity anyone reads a healthcheck at: the
// Docker default interval is 30s, Kubernetes probes are 10s or slower, and a
// human curling /healthz does not notice. A supervisor that wants tighter
// freshness sets HealthCacheTTL lower.
//
// The cached values are call and session counts. No paths, no model names, no
// titles — nothing about what is being worked on, which is the only reason the
// exemption is defensible in the first place.
const healthCacheTTL = 5 * time.Second

// healthCounts is the cached part of the health payload.
type healthCounts struct {
	calls    int
	sessions int
}

// healthCache holds the last aggregate and when it was taken.
//
// A mutex rather than an atomic pointer: the value is two ints and the read
// path is a healthcheck, so contention is not the concern — a torn read would
// be, and this cannot tear.
type healthCache struct {
	mu      sync.Mutex
	counts  healthCounts
	fetched time.Time
	valid   bool
}

func (c *healthCache) get(ttl time.Duration) (healthCounts, bool) {
	if ttl <= 0 {
		return healthCounts{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid || time.Since(c.fetched) >= ttl {
		return healthCounts{}, false
	}
	return c.counts, true
}

func (c *healthCache) put(counts healthCounts) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts = counts
	c.fetched = time.Now()
	c.valid = true
}

// healthCountsFor returns the call and session totals, from the cache when it is
// still fresh.
//
// A failed aggregate is not cached. /healthz reports ok=false for it, and a
// supervisor retries on its own schedule; caching the failure would only make a
// transient lock contention look like a down service for the length of the TTL.
func (s *Server) healthCountsFor(ctx context.Context) (healthCounts, bool) {
	if counts, ok := s.health.get(s.HealthCacheTTL); ok {
		return counts, true
	}
	totals, err := s.store.Totals(ctx, store.Filter{})
	if err != nil {
		return healthCounts{}, false
	}
	counts := healthCounts{calls: totals.Calls, sessions: totals.Sessions}
	s.health.put(counts)
	return counts, true
}
