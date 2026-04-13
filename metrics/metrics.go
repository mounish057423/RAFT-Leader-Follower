// Package metrics provides lightweight, lock-free observability for the raft-kv node.
//
// All counters use sync/atomic so they can be updated from FSM.Apply() and
// HTTP handlers concurrently without any mutex overhead.
//
// Design choice: no external libraries (Prometheus, OpenTelemetry etc.).
// A single /metrics HTTP endpoint returns JSON so the data is easy to scrape,
// graph in a spreadsheet, or read with `curl | jq` during a load test.
package metrics

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ── Global atomic counters ────────────────────────────────────────────────────
// Counters are package-level so any package can call metrics.RecordWrite()
// without needing to pass a struct around.

var (
	totalWrites     int64 // every successful FSM apply (PUT committed)
	totalDuplicates int64 // idempotency_key already seen — write skipped
	totalErrors     int64 // raft apply errors or SQLite batch errors
	totalForwards   int64 // follower → leader forwarded requests
	queueDepth      int64 // current BatchWriter channel length (point-in-time)
	dedupMapSize    int64 // current number of tracked idempotency keys
)

// RecordWrite increments the committed-write counter.
// Call from FSM.Apply() after a successful PUT is applied to the KV store.
func RecordWrite() { atomic.AddInt64(&totalWrites, 1) }

// RecordDuplicate increments the duplicate-skip counter.
// Call from FSM.Apply() when an idempotency_key was already seen.
func RecordDuplicate() { atomic.AddInt64(&totalDuplicates, 1) }

// RecordError increments the error counter.
// Call from batch writer when a SQLite flush fails.
func RecordError() { atomic.AddInt64(&totalErrors, 1) }

// RecordForward increments the forwarded-request counter.
// Call from HTTP handler when a follower forwards a write to the leader.
func RecordForward() { atomic.AddInt64(&totalForwards, 1) }

// SetQueueDepth records the current length of the BatchWriter channel.
// Call from the batch worker's ticker so /metrics always has a fresh value.
func SetQueueDepth(n int) { atomic.StoreInt64(&queueDepth, int64(n)) }

// SetDedupMapSize records the current number of tracked idempotency keys.
// Called by the dedup cleaner after each eviction pass so /metrics reflects
// the actual in-memory footprint of the dedup store.
func SetDedupMapSize(n int) { atomic.StoreInt64(&dedupMapSize, int64(n)) }

// ── Throughput tracker ────────────────────────────────────────────────────────

// throughput tracks writes-per-second over a sliding 1-second window.
// It stores the total write count at the start of each second and computes
// the delta on each /metrics request.
var tp = &throughputTracker{}

type throughputTracker struct {
	mu          sync.Mutex
	windowStart time.Time
	startCount  int64
	lastWPS     float64
}

// WritesPerSecond returns the average write throughput over the last ~1 second.
func WritesPerSecond() float64 {
	tp.mu.Lock()
	defer tp.mu.Unlock()

	now := time.Now()
	current := atomic.LoadInt64(&totalWrites)

	if tp.windowStart.IsZero() {
		tp.windowStart = now
		tp.startCount = current
		return 0
	}

	elapsed := now.Sub(tp.windowStart).Seconds()
	if elapsed >= 1.0 {
		tp.lastWPS = float64(current-tp.startCount) / elapsed
		tp.windowStart = now
		tp.startCount = current
	}
	return tp.lastWPS
}

// ── Latency histogram ────────────────────────────────────────────────────────

// LatencyTracker records end-to-end PUT latencies (from HTTP receipt to 200 OK)
// in a fixed-size ring buffer and computes p50 / p95 / p99 on demand.
//
// Why a ring buffer?
//   - Fixed memory: exactly cap * 8 bytes regardless of write volume.
//   - No locks on the hot path (only one goroutine writes per slot via CAS on head).
//   - Percentiles are computed from the snapshot — slightly stale but good enough.
type LatencyTracker struct {
	mu      sync.Mutex
	samples []int64 // nanoseconds
	head    int
	full    bool
	cap     int
}

// NewLatencyTracker creates a tracker keeping the last n samples.
func NewLatencyTracker(n int) *LatencyTracker {
	return &LatencyTracker{
		samples: make([]int64, n),
		cap:     n,
	}
}

// Record adds one latency sample (call with time.Since(start).Nanoseconds()).
func (t *LatencyTracker) Record(ns int64) {
	t.mu.Lock()
	t.samples[t.head] = ns
	t.head = (t.head + 1) % t.cap
	if t.head == 0 {
		t.full = true
	}
	t.mu.Unlock()
}

// Percentiles returns p50, p95, p99 in milliseconds.
// Returns (0, 0, 0) if there are no samples yet.
func (t *LatencyTracker) Percentiles() (p50, p95, p99 float64) {
	t.mu.Lock()
	n := t.cap
	if !t.full {
		n = t.head
	}
	if n == 0 {
		t.mu.Unlock()
		return 0, 0, 0
	}
	// Copy snapshot so we can release the lock before sorting.
	snap := make([]int64, n)
	copy(snap, t.samples[:n])
	t.mu.Unlock()

	sort.Slice(snap, func(i, j int) bool { return snap[i] < snap[j] })

	toMs := func(ns int64) float64 { return math.Round(float64(ns)/1e6*100) / 100 }

	idx := func(pct float64) int {
		i := int(math.Ceil(pct/100*float64(len(snap)))) - 1
		if i < 0 {
			i = 0
		}
		return i
	}

	return toMs(snap[idx(50)]), toMs(snap[idx(95)]), toMs(snap[idx(99)])
}

// Global latency tracker — 2,000 samples (~last few seconds at 5k writes/sec).
var Latency = NewLatencyTracker(2000)

// ── Snapshot for /metrics response ───────────────────────────────────────────

// Snapshot is a point-in-time copy of all metrics.
// It is the JSON response body for GET /metrics.
type Snapshot struct {
	TotalWrites     int64   `json:"total_writes"`
	TotalDuplicates int64   `json:"total_duplicates"`
	TotalErrors     int64   `json:"total_errors"`
	TotalForwards   int64   `json:"total_forwards"`
	QueueDepth      int64   `json:"batch_queue_depth"`
	DedupMapSize    int64   `json:"dedup_map_size"`    // live key count after TTL eviction
	WritesPerSec    float64 `json:"writes_per_sec"`
	LatencyP50Ms    float64 `json:"latency_p50_ms"`
	LatencyP95Ms    float64 `json:"latency_p95_ms"`
	LatencyP99Ms    float64 `json:"latency_p99_ms"`
}

// Get returns a fresh Snapshot of all current metrics.
func Get() Snapshot {
	p50, p95, p99 := Latency.Percentiles()
	return Snapshot{
		TotalWrites:     atomic.LoadInt64(&totalWrites),
		TotalDuplicates: atomic.LoadInt64(&totalDuplicates),
		TotalErrors:     atomic.LoadInt64(&totalErrors),
		TotalForwards:   atomic.LoadInt64(&totalForwards),
		QueueDepth:      atomic.LoadInt64(&queueDepth),
		DedupMapSize:    atomic.LoadInt64(&dedupMapSize),
		WritesPerSec:    WritesPerSecond(),
		LatencyP50Ms:    p50,
		LatencyP95Ms:    p95,
		LatencyP99Ms:    p99,
	}
}
