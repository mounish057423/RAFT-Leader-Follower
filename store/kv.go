// Package store holds the application state layers:
//   - KV  : in-memory key-value map (fast reads, rebuilt from log on restart)
//   - SQLite: durable append-only log of every committed command
package store

import "sync"

// KV is a thread-safe in-memory key-value store.
// It is the "live view" of the replicated log.
// On a fresh start it is empty; the FSM rebuilds it by replaying the SQLite log.
type KV struct {
	mu   sync.RWMutex
	data map[string]string
}

// NewKV creates an empty KV store.
func NewKV() *KV {
	return &KV{data: make(map[string]string)}
}

// Set stores value under key.
func (k *KV) Set(key, value string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.data[key] = value
}

// Get retrieves the value for key.
// Returns ("", false) if not present.
func (k *KV) Get(key string) (string, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	v, ok := k.data[key]
	return v, ok
}
