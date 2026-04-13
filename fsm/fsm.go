// Package fsm implements the hashicorp/raft FSM interface.
//
// What is an FSM?
// ---------------
// hashicorp/raft calls your FSM every time a log entry is COMMITTED
// (i.e. acknowledged by a majority of nodes). The FSM is the bridge
// between "raft decided this entry is real" and "my application acts on it."
//
// Every node runs the SAME FSM code on the SAME entries in the SAME order.
// That's how all three nodes end up with identical state.
//
// The three methods raft requires:
//   Apply()    — called for each committed entry; update your state here.
//   Snapshot() — called periodically to create a point-in-time snapshot so
//                raft doesn't have to replay the entire log forever.
//   Restore()  — called during startup or after installing a snapshot from
//                the leader; rebuild your state from the snapshot.
package fsm

import (
	"encoding/json"
	"fmt"
	"io"
	"log"

	"github.com/hashicorp/raft"
	"raft-kv/dedup"
	"raft-kv/metrics"
	"raft-kv/store"
)

// Command is the structure we encode into every raft log entry.
// It is serialised to JSON before being handed to raft, and deserialised
// inside Apply() on every node.
type Command struct {
	Op             string `json:"op"`              // "put" is the only op for now
	Key            string `json:"key"`
	Value          string `json:"value"`
	IdempotencyKey string `json:"idempotency_key"` // UUID chosen by the client
}

// ApplyResult is returned by Apply() to the leader's raft.Apply() call.
// The leader receives this back through the raft.ApplyFuture.Response().
type ApplyResult struct {
	Error string `json:"error,omitempty"`
}

// FSM is our application's finite state machine.
// It holds references to both storage layers, the dedup tracker, and the
// batch writer that decouples SQLite writes from the raft commit hot-path.
type FSM struct {
	kv          *store.KV
	db          *store.SQLiteLog
	dedup       *dedup.Store
	batchWriter *store.BatchWriter // async SQLite writer — keeps Apply() fast
	nodeID      string             // for structured log lines
}

// New creates an FSM wired up to the given storage layers.
// bw is the batch writer worker; it must already be running (call store.NewBatchWriter).
func New(kv *store.KV, db *store.SQLiteLog, dd *dedup.Store, bw *store.BatchWriter, nodeID string) *FSM {
	return &FSM{kv: kv, db: db, dedup: dd, batchWriter: bw, nodeID: nodeID}
}

// ── raft.FSM interface ────────────────────────────────────────────────────────

// Apply is called by hashicorp/raft on EVERY node after an entry is committed.
//
// Performance design (Issue 2 fix):
//   BEFORE: db.Exec(INSERT) blocked raft until SQLite finished (~1ms each).
//           At 5,000 entries/sec that is 5 full seconds of blocking per second — impossible.
//   AFTER:  KV update is in-memory (nanoseconds).
//           SQLite write is queued to BatchWriter (non-blocking channel send).
//           The worker batches 200 entries per transaction → ~200x fewer transactions.
func (f *FSM) Apply(l *raft.Log) interface{} {
	// 1. Decode the command from the raw log bytes.
	var cmd Command
	if err := json.Unmarshal(l.Data, &cmd); err != nil {
		log.Printf("[FSM] node=%s decode error idx=%d: %v", f.nodeID, l.Index, err)
		return &ApplyResult{Error: fmt.Sprintf("decode: %v", err)}
	}

	// 2. Idempotency check — skip if we already applied this key.
	if f.dedup.IsDuplicate(cmd.IdempotencyKey) {
		log.Printf("[FSM] node=%s duplicate idem_key=%s idx=%d — skipping",
			f.nodeID, cmd.IdempotencyKey, l.Index)
		metrics.RecordDuplicate()
		return &ApplyResult{}
	}

	// 3. Apply the command to the in-memory KV store (always fast — just a map write).
	switch cmd.Op {
	case "put":
		f.kv.Set(cmd.Key, cmd.Value)
	default:
		return &ApplyResult{Error: fmt.Sprintf("unknown op: %s", cmd.Op)}
	}

	// 4. Queue the SQLite write to the batch worker — does NOT block raft.
	//    The worker accumulates entries and flushes them in bulk transactions.
	//    On crash, raft will re-apply from BoltDB so no data is lost.
	f.batchWriter.Send(store.LogEntry{
		Index:          l.Index,
		IdempotencyKey: cmd.IdempotencyKey,
		Key:            cmd.Key,
		Value:          cmd.Value,
	})

	// 5. Mark the idempotency key as seen so duplicate retries are ignored.
	f.dedup.MarkSeen(cmd.IdempotencyKey)

	// 6. Record metrics — atomic, no lock, safe from any goroutine.
	metrics.RecordWrite()

	return &ApplyResult{}
}

// Snapshot creates a point-in-time snapshot of the KV state.
// hashicorp/raft calls this periodically so it can truncate old log entries
// (preventing the BoltDB raft log from growing forever).
// We read from SQLite (authoritative, durable) rather than from the in-memory
// KV so we never snapshot un-flushed batch-writer entries.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	log.Printf("[FSM] node=%s creating snapshot", f.nodeID)
	snap := &fsmSnapshot{}
	entries := make(map[string]string)
	err := f.db.Replay(func(e store.LogEntry) error {
		entries[e.Key] = e.Value
		return nil
	})
	if err != nil {
		return nil, err
	}
	snap.data = entries
	log.Printf("[FSM] node=%s snapshot ready keys=%d", f.nodeID, len(entries))
	return snap, nil
}

// Restore is called when a follower installs a snapshot from the leader.
// We MUST update BOTH the in-memory KV AND SQLite here. Without the SQLite
// update, a node that installs a snapshot and then restarts would replay stale
// SQLite data and diverge from the rest of the cluster.
func (f *FSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	log.Printf("[FSM] node=%s restoring from snapshot", f.nodeID)

	var entries map[string]string
	if err := json.NewDecoder(rc).Decode(&entries); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}

	// 1. Atomically replace the SQLite log so restarts replay correct state.
	if err := f.db.ReplaceAll(entries); err != nil {
		return fmt.Errorf("restore sqlite: %w", err)
	}

	// 2. Replace the in-memory KV.
	for k, v := range entries {
		f.kv.Set(k, v)
	}
	log.Printf("[FSM] node=%s restore complete keys=%d", f.nodeID, len(entries))
	return nil
}

// ── snapshot helper ───────────────────────────────────────────────────────────

type fsmSnapshot struct {
	data map[string]string
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if err := json.NewEncoder(sink).Encode(s.data); err != nil {
		_ = sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}
