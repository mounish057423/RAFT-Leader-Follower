// // Package server exposes the HTTP API that clients use to interact with the
// // raft cluster.
// //
// // Endpoints:
// //   POST /put      — write a key-value pair (with idempotency_key)
// //   GET  /get      — read a value by key
// //   GET  /status   — show this node's role and the current leader
// //   GET  /health   — liveness check (always 200 if the process is alive)
// //   GET  /metrics  — throughput, latency percentiles, error counts (Issue 6)
// package server

// import (
// 	"encoding/json"
// 	"fmt"
// 	"log"
// 	"net/http"
// 	"time"

// 	"raft-kv/cluster"
// 	"raft-kv/config"
// 	"raft-kv/fsm"
// 	"raft-kv/metrics"
// 	"raft-kv/store"
// )

// // maxConcurrent caps simultaneous PUT handlers to prevent unbounded goroutine
// // growth under sudden load spikes. Excess requests wait up to 5s then get 503.
// const maxConcurrent = 256

// // Server holds everything the HTTP handlers need.
// type Server struct {
// 	node      *cluster.RaftNode
// 	kv        *store.KV
// 	peers     []config.Peer
// 	httpAddrs map[string]string
// 	sem       chan struct{} // backpressure semaphore
// }

// // PutRequest is the JSON body for POST /put.
// type PutRequest struct {
// 	Key            string `json:"key"`
// 	Value          string `json:"value"`
// 	IdempotencyKey string `json:"idempotency_key"`
// }

// // New creates a Server and registers all routes on mux.
// func New(
// 	mux *http.ServeMux,
// 	node *cluster.RaftNode,
// 	kv *store.KV,
// 	peers []config.Peer,
// 	httpAddrs map[string]string,
// ) {
// 	s := &Server{
// 		node:      node,
// 		kv:        kv,
// 		peers:     peers,
// 		httpAddrs: httpAddrs,
// 		sem:       make(chan struct{}, maxConcurrent),
// 	}
// 	mux.HandleFunc("/put",     s.handlePut)
// 	mux.HandleFunc("/get",     s.handleGet)
// 	mux.HandleFunc("/status",  s.handleStatus)
// 	mux.HandleFunc("/health",  s.handleHealth)
// 	mux.HandleFunc("/metrics", s.handleMetrics)
// }

// // ── POST /put ─────────────────────────────────────────────────────────────────

// func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
// 	// Issue 6: record end-to-end latency for every PUT request.
// 	start := time.Now()
// 	defer func() {
// 		metrics.Latency.Record(time.Since(start).Nanoseconds())
// 	}()

// 	if r.Method != http.MethodPost {
// 		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
// 		return
// 	}

// 	// Backpressure — acquire semaphore slot or 503 after 5s.
// 	select {
// 	case s.sem <- struct{}{}:
// 		defer func() { <-s.sem }()
// 	case <-time.After(5 * time.Second):
// 		log.Printf("[HTTP] node=%s backpressure triggered — rejecting PUT key=%s (sem=%d/%d)",
// 			s.node.NodeID, r.URL.Query().Get("key"), maxConcurrent, maxConcurrent)
// 		http.Error(w, "server overloaded, try again", http.StatusServiceUnavailable)
// 		return
// 	}

// 	var req PutRequest
// 	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
// 		http.Error(w, fmt.Sprintf("bad request: %v", err), http.StatusBadRequest)
// 		return
// 	}
// 	if req.Key == "" {
// 		http.Error(w, "key is required", http.StatusBadRequest)
// 		return
// 	}
// 	if req.IdempotencyKey == "" {
// 		http.Error(w, "idempotency_key is required", http.StatusBadRequest)
// 		return
// 	}

// 	// Follower → forward to leader with retry.
// 	if !s.node.IsLeader() {
// 		metrics.RecordForward()
// 		s.forwardWithRetry(w, req)
// 		return
// 	}

// 	s.applyAsLeader(w, req)
// }

// // applyAsLeader submits the command to raft. If leadership was lost between
// // the IsLeader() check and Apply() (ErrNotLeader race), we forward instead
// // of returning an error.
// func (s *Server) applyAsLeader(w http.ResponseWriter, req PutRequest) {
// 	cmd := fsm.Command{
// 		Op:             "put",
// 		Key:            req.Key,
// 		Value:          req.Value,
// 		IdempotencyKey: req.IdempotencyKey,
// 	}
// 	data, err := json.Marshal(cmd)
// 	if err != nil {
// 		http.Error(w, "encode error", http.StatusInternalServerError)
// 		return
// 	}

// 	applied, err := s.node.Apply(data, 5*time.Second)
// 	if err != nil {
// 		metrics.RecordError()
// 		log.Printf("[HTTP] node=%s role=leader apply error key=%s: %v",
// 			s.node.NodeID, req.Key, err)
// 		http.Error(w, fmt.Sprintf("raft apply: %v", err), http.StatusInternalServerError)
// 		return
// 	}
// 	if !applied {
// 		// ErrNotLeader race — forward to the real leader.
// 		_, leaderID := s.node.Raft.LeaderWithID()
// 		log.Printf("[HTTP] node=%s lost leadership during apply key=%s — forwarding to leader=%s",
// 			s.node.NodeID, req.Key, leaderID)
// 		metrics.RecordForward()
// 		s.forwardWithRetry(w, req)
// 		return
// 	}

// 	w.WriteHeader(http.StatusOK)
// }

// // forwardWithRetry forwards the request to the current leader.
// // 3 attempts with back-off handle the case where the leader changes
// // mid-request (network partition, crash, re-election). On each retry
// // we re-resolve the leader address so we always target the actual leader.
// func (s *Server) forwardWithRetry(w http.ResponseWriter, req PutRequest) {
// 	const attempts = 3
// 	for i := 0; i < attempts; i++ {
// 		leaderHTTP := s.node.LeaderHTTPAddr(s.peers, s.httpAddrs)
// 		_, leaderID := s.node.Raft.LeaderWithID()
// 		if leaderHTTP == "" {
// 			if i < attempts-1 {
// 				log.Printf("[HTTP] node=%s no leader yet (attempt %d/%d) — waiting",
// 					s.node.NodeID, i+1, attempts)
// 				time.Sleep(150 * time.Millisecond)
// 				continue
// 			}
// 			http.Error(w, "no leader elected — try again shortly", http.StatusServiceUnavailable)
// 			return
// 		}

// 		log.Printf("[HTTP] node=%s role=follower forwarding key=%s to leader=%s addr=%s (attempt %d/%d)",
// 			s.node.NodeID, req.Key, leaderID, leaderHTTP, i+1, attempts)
// 		if err := forwardToLeader(leaderHTTP, req); err != nil {
// 			if i < attempts-1 {
// 				time.Sleep(100 * time.Millisecond)
// 				continue
// 			}
// 			http.Error(w, fmt.Sprintf("forward error: %v", err), http.StatusBadGateway)
// 			return
// 		}
// 		w.WriteHeader(http.StatusOK)
// 		return
// 	}
// }

// // ── GET /get?key=... ──────────────────────────────────────────────────────────

// func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
// 	if r.Method != http.MethodGet {
// 		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
// 		return
// 	}
// 	key := r.URL.Query().Get("key")
// 	if key == "" {
// 		http.Error(w, "key query param required", http.StatusBadRequest)
// 		return
// 	}
// 	value, ok := s.kv.Get(key)
// 	if !ok {
// 		http.Error(w, "key not found", http.StatusNotFound)
// 		return
// 	}
// 	w.Header().Set("Content-Type", "application/json")
// 	json.NewEncoder(w).Encode(map[string]string{"key": key, "value": value})
// }

// // ── GET /status ───────────────────────────────────────────────────────────────

// func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
// 	leaderAddr, leaderID := s.node.Raft.LeaderWithID()
// 	role := "follower"
// 	if s.node.IsLeader() {
// 		role = "leader"
// 	}
// 	w.Header().Set("Content-Type", "application/json")
// 	json.NewEncoder(w).Encode(map[string]string{
// 		"node_id":     s.node.NodeID,
// 		"role":        role,
// 		"leader_id":   string(leaderID),
// 		"leader_raft": string(leaderAddr),
// 		"leader_http": s.httpAddrs[string(leaderID)],
// 	})
// }

// // ── GET /health ───────────────────────────────────────────────────────────────

// func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
// 	w.Header().Set("Content-Type", "application/json")
// 	json.NewEncoder(w).Encode(map[string]string{
// 		"status":  "ok",
// 		"node_id": s.node.NodeID,
// 		"role":    s.node.Raft.State().String(),
// 	})
// }

// // ── GET /metrics ──────────────────────────────────────────────────────────────
// //
// // Issue 6 — metrics endpoint.
// // Returns a JSON object with all observable metrics for this node.
// //
// // Example response:
// //
// //	{
// //	  "total_writes":      12345,
// //	  "total_duplicates":  3,
// //	  "total_errors":      0,
// //	  "total_forwards":    100,
// //	  "batch_queue_depth": 12,
// //	  "writes_per_sec":    4823.5,
// //	  "latency_p50_ms":    1.2,
// //	  "latency_p95_ms":    3.8,
// //	  "latency_p99_ms":    9.1
// //	}
// //
// // Use during load tests to watch throughput in real time:
// //
// //	watch -n1 'curl -s http://127.0.0.1:8001/metrics | jq'
// func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
// 	snap := metrics.Get()
// 	w.Header().Set("Content-Type", "application/json")
// 	json.NewEncoder(w).Encode(snap)
// }

// Package server exposes the HTTP API that clients use to interact with the
// raft cluster.
//
// Endpoints:
//   POST /put      — write a key-value pair (with idempotency_key)
//   GET  /get      — read a value by key
//   GET  /status   — show this node's role and the current leader
//   GET  /health   — liveness check (always 200 if the process is alive)
//   GET  /metrics  — throughput, latency percentiles, error counts (Issue 6)
//   GET  /swagger/ — Swagger UI (open in browser)
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"raft-kv/cluster"
	"raft-kv/config"
	"raft-kv/fsm"
	"raft-kv/metrics"
	"raft-kv/store"
)

// maxConcurrent caps simultaneous PUT handlers to prevent unbounded goroutine
// growth under sudden load spikes. Excess requests wait up to 5s then get 503.
const maxConcurrent = 256

// Server holds everything the HTTP handlers need.
type Server struct {
	node      *cluster.RaftNode
	kv        *store.KV
	peers     []config.Peer
	httpAddrs map[string]string
	sem       chan struct{} // backpressure semaphore
}

// PutRequest is the JSON body for POST /put.
type PutRequest struct {
	Key            string `json:"key"`
	Value          string `json:"value"`
	IdempotencyKey string `json:"idempotency_key"`
}

// New creates a Server and registers all routes on mux.
func New(
	mux *http.ServeMux,
	node *cluster.RaftNode,
	kv *store.KV,
	peers []config.Peer,
	httpAddrs map[string]string,
) {
	s := &Server{
		node:      node,
		kv:        kv,
		peers:     peers,
		httpAddrs: httpAddrs,
		sem:       make(chan struct{}, maxConcurrent),
	}
	mux.HandleFunc("/put",     s.handlePut)
	mux.HandleFunc("/get",     s.handleGet)
	mux.HandleFunc("/status",  s.handleStatus)
	mux.HandleFunc("/health",  s.handleHealth)
	mux.HandleFunc("/metrics", s.handleMetrics)

	// Swagger UI — open http://127.0.0.1:8001/swagger/ in your browser.
	registerSwagger(mux)
}

// ── POST /put ─────────────────────────────────────────────────────────────────

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
	// Issue 6: record end-to-end latency for every PUT request.
	start := time.Now()
	defer func() {
		metrics.Latency.Record(time.Since(start).Nanoseconds())
	}()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Backpressure — acquire semaphore slot or 503 after 5s.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-time.After(5 * time.Second):
		log.Printf("[HTTP] node=%s backpressure triggered — rejecting PUT key=%s (sem=%d/%d)",
			s.node.NodeID, r.URL.Query().Get("key"), maxConcurrent, maxConcurrent)
		http.Error(w, "server overloaded, try again", http.StatusServiceUnavailable)
		return
	}

	var req PutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("bad request: %v", err), http.StatusBadRequest)
		return
	}
	if req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	if req.IdempotencyKey == "" {
		http.Error(w, "idempotency_key is required", http.StatusBadRequest)
		return
	}

	// Follower → forward to leader with retry.
	if !s.node.IsLeader() {
		metrics.RecordForward()
		s.forwardWithRetry(w, req)
		return
	}

	s.applyAsLeader(w, req)
}

// applyAsLeader submits the command to raft. If leadership was lost between
// the IsLeader() check and Apply() (ErrNotLeader race), we forward instead
// of returning an error.
func (s *Server) applyAsLeader(w http.ResponseWriter, req PutRequest) {
	cmd := fsm.Command{
		Op:             "put",
		Key:            req.Key,
		Value:          req.Value,
		IdempotencyKey: req.IdempotencyKey,
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
		return
	}

	applied, err := s.node.Apply(data, 5*time.Second)
	if err != nil {
		metrics.RecordError()
		log.Printf("[HTTP] node=%s role=leader apply error key=%s: %v",
			s.node.NodeID, req.Key, err)
		http.Error(w, fmt.Sprintf("raft apply: %v", err), http.StatusInternalServerError)
		return
	}
	if !applied {
		// ErrNotLeader race — forward to the real leader.
		_, leaderID := s.node.Raft.LeaderWithID()
		log.Printf("[HTTP] node=%s lost leadership during apply key=%s — forwarding to leader=%s",
			s.node.NodeID, req.Key, leaderID)
		metrics.RecordForward()
		s.forwardWithRetry(w, req)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// forwardWithRetry forwards the request to the current leader.
// 3 attempts with back-off handle the case where the leader changes
// mid-request (network partition, crash, re-election). On each retry
// we re-resolve the leader address so we always target the actual leader.
func (s *Server) forwardWithRetry(w http.ResponseWriter, req PutRequest) {
	const attempts = 3
	for i := 0; i < attempts; i++ {
		leaderHTTP := s.node.LeaderHTTPAddr(s.peers, s.httpAddrs)
		_, leaderID := s.node.Raft.LeaderWithID()
		if leaderHTTP == "" {
			if i < attempts-1 {
				log.Printf("[HTTP] node=%s no leader yet (attempt %d/%d) — waiting",
					s.node.NodeID, i+1, attempts)
				time.Sleep(150 * time.Millisecond)
				continue
			}
			http.Error(w, "no leader elected — try again shortly", http.StatusServiceUnavailable)
			return
		}

		log.Printf("[HTTP] node=%s role=follower forwarding key=%s to leader=%s addr=%s (attempt %d/%d)",
			s.node.NodeID, req.Key, leaderID, leaderHTTP, i+1, attempts)
		if err := forwardToLeader(leaderHTTP, req); err != nil {
			if i < attempts-1 {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			http.Error(w, fmt.Sprintf("forward error: %v", err), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
}

// ── GET /get?key=... ──────────────────────────────────────────────────────────

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "key query param required", http.StatusBadRequest)
		return
	}
	value, ok := s.kv.Get(key)
	if !ok {
		http.Error(w, "key not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"key": key, "value": value})
}

// ── GET /status ───────────────────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	leaderAddr, leaderID := s.node.Raft.LeaderWithID()
	role := "follower"
	if s.node.IsLeader() {
		role = "leader"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"node_id":     s.node.NodeID,
		"role":        role,
		"leader_id":   string(leaderID),
		"leader_raft": string(leaderAddr),
		"leader_http": s.httpAddrs[string(leaderID)],
	})
}

// ── GET /health ───────────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"node_id": s.node.NodeID,
		"role":    s.node.Raft.State().String(),
	})
}

// ── GET /metrics ──────────────────────────────────────────────────────────────

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	snap := metrics.Get()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snap)
}
