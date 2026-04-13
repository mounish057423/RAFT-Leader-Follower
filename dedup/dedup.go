// Package dedup provides idempotent write protection.
//
// Every PUT command carries an idempotency_key (a UUID string).
// Before applying a log entry the FSM checks this store. If the key was
// already applied we silently skip it, preventing duplicate application of
// the same command even when clients retry.
//
// Memory management (Issue 4 fix)
// --------------------------------
// A plain map[string]bool grows forever — one entry per unique write. At
// millions of writes this eventually OOMs the process. We fix this with a
// time-stamped map and a background eviction goroutine.
//
// Each key is stored alongside the time it was first seen. A goroutine wakes
// every cleanInterval and removes keys older than ttl. The TTL must be
// larger than the longest reasonable client retry window. We default to
// 10 minutes (configurable via DEDUP_TTL_MINUTES) which is conservative:
// if a client has not retried within 10 minutes it won't retry at all.
//
// Correctness guarantee: the eviction window matches the meaningful retry
// window. A client retrying after 10+ minutes is making a brand-new write,
// and treating it as one is correct.
package dedup

import (
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"raft-kv/metrics"
)

// entry records a key and when it was first seen.
type entry struct {
	seenAt time.Time
}

// Store tracks which idempotency keys have already been applied.
type Store struct {
	mu            sync.RWMutex
	seen          map[string]entry
	ttl           time.Duration
	cleanInterval time.Duration
	stop          chan struct{}
}

// New creates an empty dedup store and starts the background eviction goroutine.
//
// TTL is read from DEDUP_TTL_MINUTES (default 10).
// Clean interval is read from DEDUP_CLEAN_INTERVAL_MINUTES (default 1).
func New() *Store {
	ttlMin   := envInt("DEDUP_TTL_MINUTES",            10)
	cleanMin := envInt("DEDUP_CLEAN_INTERVAL_MINUTES",  1)

	s := &Store{
		seen:          make(map[string]entry),
		ttl:           time.Duration(ttlMin) * time.Minute,
		cleanInterval: time.Duration(cleanMin) * time.Minute,
		stop:          make(chan struct{}),
	}

	log.Printf("[Dedup] TTL=%v cleanInterval=%v", s.ttl, s.cleanInterval)
	go s.cleaner()
	return s
}

// Stop shuts down the background cleaner goroutine.
// Call during graceful shutdown after raft has stopped delivering new entries.
func (s *Store) Stop() {
	close(s.stop)
}

// cleaner is the background eviction goroutine.
// It wakes every cleanInterval and removes expired entries.
func (s *Store) cleaner() {
	ticker := time.NewTicker(s.cleanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.evict()
		case <-s.stop:
			return
		}
	}
}

// evict deletes all entries whose seenAt is older than ttl.
//
// At 5,000 writes/sec with a 10-minute TTL the map holds at most ~3M entries.
// A Go map scan over 3M keys completes in <50ms, well within the 1-minute
// eviction interval, so the write-lock hold time is acceptable.
func (s *Store) evict() {
	cutoff := time.Now().Add(-s.ttl)

	s.mu.Lock()
	before := len(s.seen)
	for k, e := range s.seen {
		if e.seenAt.Before(cutoff) {
			delete(s.seen, k)
		}
	}
	after := len(s.seen)
	s.mu.Unlock()

	if removed := before - after; removed > 0 {
		log.Printf("[Dedup] evicted %d expired keys (size: %d → %d)", removed, before, after)
	}
	metrics.SetDedupMapSize(after)
}

// IsDuplicate returns true if key was already marked as seen and has not
// yet been evicted. Safe for concurrent use.
func (s *Store) IsDuplicate(key string) bool {
	if key == "" {
		return false // empty key means caller opted out of dedup
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.seen[key]
	return ok
}

// MarkSeen records that key was applied at the current time.
// Safe for concurrent use.
func (s *Store) MarkSeen(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen[key] = entry{seenAt: time.Now()}
}

// Len returns the number of currently tracked (non-evicted) keys.
// Useful for tests and the /metrics endpoint.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.seen)
}

// envInt reads an integer env var, returning defaultVal if unset or invalid.
func envInt(name string, defaultVal int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		log.Printf("[Dedup] invalid %s=%q — using default %d", name, v, defaultVal)
	}
	return defaultVal
}
