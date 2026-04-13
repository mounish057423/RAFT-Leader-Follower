package store

import (
	"database/sql"
	"log"
	"os"
	"strconv"
	"time"

	"raft-kv/metrics"
)

// BatchWriter decouples SQLite writes from the raft commit hot-path.
//
// FSM.Apply() queues entries to a buffered channel instead of calling
// db.Exec() inline. A background worker drains the channel in bulk
// transactions, giving ~200x fewer transactions per second.
//
// Issue 2 fix — controlled backpressure (not silent drop):
//   WRONG  → select { default: } — fires immediately, could miss entries
//   FIXED  → select { case <-time.After(100ms): } — gives the worker time
//             to drain before falling back to a synchronous write.
//             Entries are NEVER silently dropped.
//
// Issue 3 fix — env-configurable tuning:
//   BATCH_SIZE        default 200   — rows per SQLite transaction
//   FLUSH_INTERVAL_MS default 10   — max ms between flushes
//   BATCH_CHAN_SIZE   default 4096  — channel buffer depth
type BatchWriter struct {
	ch            chan LogEntry
	stop          chan struct{}
	db            *sql.DB
	flushSize     int           // rows per transaction (Issue 3)
	flushInterval time.Duration // max time between flushes (Issue 3)
}

// NewBatchWriter reads tuning parameters from environment variables so
// operators can adjust throughput vs latency trade-offs without recompiling.
//
//	BATCH_SIZE        — larger = fewer transactions = higher throughput, more latency
//	FLUSH_INTERVAL_MS — smaller = lower latency, more transactions
//	BATCH_CHAN_SIZE    — larger = absorbs bigger bursts without blocking Apply()
func NewBatchWriter(sqliteLog *SQLiteLog) *BatchWriter {
	chanSize  := envInt("BATCH_CHAN_SIZE",    4096)
	flushSize := envInt("BATCH_SIZE",          200)
	flushMs   := envInt("FLUSH_INTERVAL_MS",    10)

	log.Printf("[BatchWriter] chanSize=%d flushSize=%d flushInterval=%dms",
		chanSize, flushSize, flushMs)

	bw := &BatchWriter{
		ch:            make(chan LogEntry, chanSize),
		stop:          make(chan struct{}),
		db:            sqliteLog.db,
		flushSize:     flushSize,
		flushInterval: time.Duration(flushMs) * time.Millisecond,
	}
	go bw.run()
	return bw
}

// envInt reads an integer from an env var, returning defaultVal if unset or invalid.
func envInt(name string, defaultVal int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		log.Printf("[BatchWriter] invalid %s=%q — using default %d", name, v, defaultVal)
	}
	return defaultVal
}

// Send enqueues one log entry for asynchronous batch insertion.
//
// Issue 2 fix — backpressure with controlled timeout (not silent drop):
//   We try to queue the entry immediately. If the channel is full (worker
//   is behind), we wait up to 100ms for a slot to open. This gives the
//   batch worker time to flush and catch up. If still full after 100ms,
//   we fall back to a direct synchronous write — NEVER drop the entry.
//
// Why not block forever?
//   FSM.Apply() is called by raft on its commit goroutine. If we block
//   indefinitely we stall raft for all nodes. The 100ms timeout is long
//   enough to absorb short SQLite stalls but short enough not to stall raft.
func (bw *BatchWriter) Send(e LogEntry) {
	select {
	case bw.ch <- e:
		// Fast path — queued successfully.
		metrics.SetQueueDepth(len(bw.ch))

	case <-time.After(100 * time.Millisecond):
		// Slow path — channel was full for 100ms. Fall back to sync write.
		// This should be rare: it means the worker is >4096 entries behind.
		log.Printf("[BatchWriter] channel congested (depth=%d), falling back to sync write idx=%d",
			len(bw.ch), e.Index)
		metrics.RecordError()
		if err := bw.insertOne(e); err != nil {
			log.Printf("[BatchWriter] sync fallback error: %v", err)
		}
	}
}

// Stop signals the worker to drain remaining entries and exit cleanly.
// Call during graceful shutdown BEFORE closing the SQLite connection.
func (bw *BatchWriter) Stop() {
	close(bw.stop)
}

// run is the background worker goroutine.
func (bw *BatchWriter) run() {
	ticker := time.NewTicker(bw.flushInterval)
	defer ticker.Stop()

	batch := make([]LogEntry, 0, bw.flushSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := bw.insertBatch(batch); err != nil {
			log.Printf("[BatchWriter] batch insert error (size=%d): %v", len(batch), err)
			metrics.RecordError()
		}
		batch = batch[:0] // reset slice without re-allocating
		metrics.SetQueueDepth(len(bw.ch))
	}

	for {
		select {
		case e := <-bw.ch:
			batch = append(batch, e)
			if len(batch) >= bw.flushSize {
				flush() // flush immediately when batch is full
			}

		case <-ticker.C:
			flush() // periodic flush — keeps latency bounded

		case <-bw.stop:
			// Drain anything still in the channel before exiting.
			for {
				select {
				case e := <-bw.ch:
					batch = append(batch, e)
				default:
					flush()
					log.Printf("[BatchWriter] drained and stopped.")
					return
				}
			}
		}
	}
}

// insertBatch writes all entries in one SQLite transaction.
// One transaction for N rows costs almost the same as one row — this is
// the core reason batch writes dramatically outperform per-row inserts.
func (bw *BatchWriter) insertBatch(batch []LogEntry) error {
	tx, err := bw.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.Prepare(
		`INSERT OR IGNORE INTO kv_log (idx, idempotency_key, key, value) VALUES (?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range batch {
		if _, err := stmt.Exec(e.Index, e.IdempotencyKey, e.Key, e.Value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// insertOne is the synchronous fallback used when the channel is congested.
func (bw *BatchWriter) insertOne(e LogEntry) error {
	_, err := bw.db.Exec(
		`INSERT OR IGNORE INTO kv_log (idx, idempotency_key, key, value) VALUES (?, ?, ?, ?)`,
		e.Index, e.IdempotencyKey, e.Key, e.Value,
	)
	return err
}
