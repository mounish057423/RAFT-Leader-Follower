package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no cgo required
)

// SQLiteLog is the durable append-only log backed by SQLite.
// Every committed raft entry is written here so that data survives crashes.
// The KV store is rebuilt from this table on restart.
//
// WAL mode is enabled so concurrent readers don't block the writer,
// which is essential for hitting ≥ 5,000 entries/sec.
type SQLiteLog struct {
	db *sql.DB
}

// LogEntry is one row in the SQLite log table.
type LogEntry struct {
	Index          uint64
	IdempotencyKey string
	Key            string
	Value          string
}

// NewSQLiteLog opens (or creates) the SQLite database at path and
// initialises the schema + WAL mode.
func NewSQLiteLog(path string) (*SQLiteLog, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// WAL (Write-Ahead Log) mode lets readers and the single writer work
	// concurrently without blocking each other. Without this we cannot reach
	// 5k writes/sec because every write locks the whole file.
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		return nil, fmt.Errorf("enable WAL: %w", err)
	}

	// synchronous=NORMAL is safe with WAL and much faster than FULL.
	if _, err := db.Exec(`PRAGMA synchronous=NORMAL;`); err != nil {
		return nil, fmt.Errorf("set synchronous: %w", err)
	}

	// cache_size=10000 keeps 10,000 pages (~40MB) in memory, reducing disk I/O
	// for repeated reads of the same pages during Replay() on startup.
	if _, err := db.Exec(`PRAGMA cache_size=10000;`); err != nil {
		return nil, fmt.Errorf("set cache_size: %w", err)
	}

	// temp_store=MEMORY keeps SQLite's internal temp tables in RAM instead of
	// writing them to disk. Speeds up ORDER BY and GROUP BY in Replay().
	if _, err := db.Exec(`PRAGMA temp_store=MEMORY;`); err != nil {
		return nil, fmt.Errorf("set temp_store: %w", err)
	}

	// mmap_size=268435456 enables memory-mapped I/O for the first 256MB of the
	// database. Reads bypass the kernel page cache for lower latency.
	if _, err := db.Exec(`PRAGMA mmap_size=268435456;`); err != nil {
		return nil, fmt.Errorf("set mmap_size: %w", err)
	}

	// One writer at a time; readers don't count against this limit.
	db.SetMaxOpenConns(1)

	if err := createSchema(db); err != nil {
		return nil, err
	}

	return &SQLiteLog{db: db}, nil
}

func createSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS kv_log (
			idx             INTEGER PRIMARY KEY,
			idempotency_key TEXT NOT NULL DEFAULT '',
			key             TEXT NOT NULL,
			value           TEXT NOT NULL
		);
	`)
	return err
}

// Append writes one committed log entry to SQLite.
// Called from FSM.Apply() on every node after raft commits the entry.
func (s *SQLiteLog) Append(entry LogEntry) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO kv_log (idx, idempotency_key, key, value) VALUES (?, ?, ?, ?)`,
		entry.Index, entry.IdempotencyKey, entry.Key, entry.Value,
	)
	return err
}

// Replay reads all committed entries in order and calls fn for each one.
// Used at startup to rebuild the in-memory KV store from durable storage.
func (s *SQLiteLog) Replay(fn func(LogEntry) error) error {
	rows, err := s.db.Query(
		`SELECT idx, idempotency_key, key, value FROM kv_log ORDER BY idx ASC`,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.Index, &e.IdempotencyKey, &e.Key, &e.Value); err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Close shuts down the database connection cleanly.
func (s *SQLiteLog) Close() error {
	return s.db.Close()
}

// ReplaceAll wipes the existing log and writes a fresh set of entries.
// Called by FSM.Restore() when raft installs a snapshot on this node.
// Without this, a node that installs a snapshot and then restarts would
// replay a stale SQLite log instead of the authoritative snapshot state,
// causing the in-memory KV to diverge from what raft last delivered.
func (s *SQLiteLog) ReplaceAll(entries map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Wipe the existing log completely.
	if _, err := tx.Exec(`DELETE FROM kv_log`); err != nil {
		return fmt.Errorf("delete old log: %w", err)
	}

	// Insert the snapshot entries with synthetic indices starting at 0.
	// The exact index values don't matter here — we only need the key/value
	// pairs to be present so Replay() can rebuild KV on the next restart.
	i := 0
	for k, v := range entries {
		if _, err := tx.Exec(
			`INSERT INTO kv_log (idx, idempotency_key, key, value) VALUES (?, '', ?, ?)`,
			i, k, v,
		); err != nil {
			return fmt.Errorf("insert snapshot entry: %w", err)
		}
		i++
	}

	return tx.Commit()
}
