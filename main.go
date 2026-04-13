// main.go wires all components together and starts a single raft node.
//
// Configuration — flags take priority, env vars are the fallback:
//
//	FLAG        ENV VAR       DEFAULT           DESCRIPTION
//	-id         NODE_ID       (required)        unique node ID
//	-raft       RAFT_ADDR     127.0.0.1:7001    raft peer TCP address
//	-http       HTTP_ADDR     127.0.0.1:8001    HTTP API listen address
//	-data       DATA_DIR      ./data/node1      data directory
//	-bootstrap  BOOTSTRAP     false             bootstrap a new cluster
//
// First-time startup (all 3 terminals):
//
//	go run . -id node1 -raft 127.0.0.1:7001 -http 127.0.0.1:8001 -data ./data/node1 -bootstrap
//	go run . -id node2 -raft 127.0.0.1:7002 -http 127.0.0.1:8002 -data ./data/node2 -bootstrap
//	go run . -id node3 -raft 127.0.0.1:7003 -http 127.0.0.1:8003 -data ./data/node3 -bootstrap
//
// Subsequent restarts — omit -bootstrap (cluster state loaded from BoltDB):
//
//	go run . -id node1 -raft 127.0.0.1:7001 -http 127.0.0.1:8001 -data ./data/node1
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"raft-kv/cluster"
	"raft-kv/config"
	"raft-kv/dedup"
	"raft-kv/fsm"
	"raft-kv/server"
	"raft-kv/store"
)

// envOr returns the value of the environment variable name if set, or
// fallback otherwise. This lets operators configure nodes via env vars
// (e.g. in Docker / Kubernetes) without changing command-line invocations.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func main() {
	// ── Configuration: flags with env-var fallbacks ───────────────────────────
	nodeID    := flag.String("id",        envOr("NODE_ID",    ""),                  "unique node ID (e.g. node1) [env: NODE_ID]")
	raftAddr  := flag.String("raft",      envOr("RAFT_ADDR",  "127.0.0.1:7001"),    "raft peer TCP address [env: RAFT_ADDR]")
	httpAddr  := flag.String("http",      envOr("HTTP_ADDR",  "127.0.0.1:8001"),    "HTTP API address [env: HTTP_ADDR]")
	dataDir   := flag.String("data",      envOr("DATA_DIR",   "./data/node1"),      "data directory [env: DATA_DIR]")
	bootstrap := flag.Bool("bootstrap",   os.Getenv("BOOTSTRAP") == "true",         "bootstrap new cluster (first run only) [env: BOOTSTRAP=true]")
	flag.Parse()

	if *nodeID == "" {
		log.Fatal("flag -id (or env NODE_ID) is required")
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("Starting node=%s  raft=%s  http=%s  data=%s  bootstrap=%v",
		*nodeID, *raftAddr, *httpAddr, *dataDir, *bootstrap)

	// ── Data directory ────────────────────────────────────────────────────────
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	// ── SQLite durable log ────────────────────────────────────────────────────
	sqlitePath := filepath.Join(*dataDir, "kv.db")
	sqliteLog, err := store.NewSQLiteLog(sqlitePath)
	if err != nil {
		log.Fatalf("open sqlite: %v", err)
	}

	// ── Batch writer (Issue 2 fix) ────────────────────────────────────────────
	// The batch writer runs a background goroutine that accumulates SQLite writes
	// and flushes them in bulk transactions. FSM.Apply() sends to a channel
	// instead of writing synchronously, so raft is never blocked waiting for disk.
	batchWriter := store.NewBatchWriter(sqliteLog)

	// ── In-memory KV — rebuilt from SQLite on startup (crash recovery) ────────
	kvStore := store.NewKV()
	log.Printf("Replaying committed log from SQLite…")
	if err := sqliteLog.Replay(func(e store.LogEntry) error {
		kvStore.Set(e.Key, e.Value)
		return nil
	}); err != nil {
		log.Fatalf("replay sqlite: %v", err)
	}
	log.Printf("Replay complete.")

	// ── Dedup store ───────────────────────────────────────────────────────────
	dedupStore := dedup.New()

	// ── FSM ───────────────────────────────────────────────────────────────────
	stateMachine := fsm.New(kvStore, sqliteLog, dedupStore, batchWriter, *nodeID)

	// ── Cluster config ────────────────────────────────────────────────────────
	peers := config.DefaultClusterPeers()

	httpAddrs := map[string]string{
		"node1": "127.0.0.1:8001",
		"node2": "127.0.0.1:8002",
		"node3": "127.0.0.1:8003",
	}

	nodeCfg := &config.NodeConfig{
		NodeID:    *nodeID,
		RaftAddr:  *raftAddr,
		HTTPAddr:  *httpAddr,
		DataDir:   *dataDir,
		Peers:     peers,
		Bootstrap: *bootstrap,
	}

	// ── Start raft node ───────────────────────────────────────────────────────
	raftNode, err := cluster.New(nodeCfg, stateMachine)
	if err != nil {
		log.Fatalf("start raft node: %v", err)
	}

	// ── HTTP server ───────────────────────────────────────────────────────────
	mux := http.NewServeMux()
	server.New(mux, raftNode, kvStore, peers, httpAddrs)

	httpServer := &http.Server{
		Addr:         *httpAddr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	// Listen for SIGINT (Ctrl-C) or SIGTERM (container stop / kill).
	// When received:
	//   1. Stop accepting new HTTP connections (drain in-flight requests).
	//   2. Stop the batch writer (flush remaining SQLite entries).
	//   3. Shutdown raft (send LeaveCluster RPC so peers know we're gone).
	//   4. Close SQLite.
	// This prevents data loss and prevents other nodes from waiting for a dead
	// peer before electing a new leader.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("HTTP server listening on %s", *httpAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	// Block until shutdown signal received.
	sig := <-quit
	log.Printf("Received signal %v — starting graceful shutdown…", sig)

	// Give in-flight HTTP requests up to 10 seconds to finish.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP shutdown error: %v", err)
	}
	log.Printf("HTTP server stopped.")

	// Stop the dedup TTL cleaner — no more entries will arrive after raft shuts down.
	dedupStore.Stop()
	log.Printf("Dedup cleaner stopped.")

	// Drain and stop the SQLite batch writer.
	batchWriter.Stop()
	log.Printf("Batch writer stopped.")

	// Gracefully leave the raft cluster so peers don't wait for us.
	shutdownFuture := raftNode.Raft.Shutdown()
	if err := shutdownFuture.Error(); err != nil {
		log.Printf("Raft shutdown error: %v", err)
	}
	log.Printf("Raft shutdown complete.")

	// Close SQLite last (batch writer must be stopped first).
	if err := sqliteLog.Close(); err != nil {
		log.Printf("SQLite close error: %v", err)
	}
	log.Printf("Node %s shut down cleanly.", *nodeID)
}
