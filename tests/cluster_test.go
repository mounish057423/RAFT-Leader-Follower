// Package tests runs integration tests against an in-process 3-node raft cluster.
//
// Each test spins up 3 nodes inside the same process using real hashicorp/raft
// instances but with in-memory transports (no real TCP sockets needed).
// This makes the tests fast, deterministic, and self-contained.
package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	"raft-kv/cluster"
	"raft-kv/config"
	"raft-kv/dedup"
	"raft-kv/fsm"
	"raft-kv/server"
	"raft-kv/store"
)

// ── Test cluster helpers ───────────────────────────────────────────────────────

// testNode is one node in an in-process test cluster.
type testNode struct {
	raftNode  *cluster.RaftNode
	kv        *store.KV
	sqliteLog *store.SQLiteLog
	dedupSt   *dedup.Store
	httpSrv   *httptest.Server
	dataDir   string
}

// newTestCluster creates a 3-node raft cluster entirely in memory.
// Each node gets a real raft instance but uses an in-memory transport so no
// actual TCP connections are made.
//
// Two-phase construction is required for correct follower forwarding:
//   Phase 1 — build every raft instance and httptest.Server so we know
//              each node's real URL (e.g. http://127.0.0.1:PORT).
//   Phase 2 — wire the shared httpAddrs map (nodeID → real URL) into
//              every node's HTTP handler AFTER all servers are started.
//
// Without Phase 2 the httpAddrs would contain placeholder strings like
// "node1-http" and forwardToLeader() would build "http://node1-http/put",
// which is not routable — causing TestFollowerForward to fail at the
// network layer rather than testing the actual forwarding logic.
func newTestCluster(t *testing.T) []*testNode {
	t.Helper()

	const n = 3
	peers := make([]config.Peer, n)
	addrs := make([]raft.ServerAddress, n)
	transports := make([]*raft.InmemTransport, n)

	// Create in-memory transports and assign fake raft addresses.
	for i := 0; i < n; i++ {
		addr, trans := raft.NewInmemTransport(raft.ServerAddress(fmt.Sprintf("node%d", i+1)))
		addrs[i] = addr
		transports[i] = trans
		peers[i] = config.Peer{
			ID:       fmt.Sprintf("node%d", i+1),
			RaftAddr: string(addr),
		}
	}

	// Wire transports together so they can send raft RPCs to each other.
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i != j {
				transports[i].Connect(addrs[j], transports[j])
			}
		}
	}

	// ── Phase 1: build nodes ──────────────────────────────────────────────────
	// httpAddrs is a shared map that all nodes read for leader forwarding.
	// We fill it in Phase 2 once every httptest.Server URL is known.
	httpAddrs := make(map[string]string, n) // filled in Phase 2

	nodes := make([]*testNode, n)

	for i := 0; i < n; i++ {
		// Temp directory for SQLite.
		dir, err := os.MkdirTemp("", fmt.Sprintf("raft-test-node%d-*", i+1))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })

		sqliteLog, err := store.NewSQLiteLog(dir + "/kv.db")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sqliteLog.Close() })

		kvStore := store.NewKV()
		dd := dedup.New()
		t.Cleanup(func() { dd.Stop() })
		bw := store.NewBatchWriter(sqliteLog)
		t.Cleanup(func() { bw.Stop() })
		stateMachine := fsm.New(kvStore, sqliteLog, dd, bw, peers[i].ID)

		// Raft config — shorter timeouts keep tests fast.
		raftCfg := raft.DefaultConfig()
		raftCfg.LocalID = raft.ServerID(peers[i].ID)
		raftCfg.HeartbeatTimeout = 500 * time.Millisecond
		raftCfg.ElectionTimeout = 1000 * time.Millisecond
		raftCfg.LeaderLeaseTimeout = 500 * time.Millisecond
		raftCfg.CommitTimeout = 20 * time.Millisecond
		raftCfg.Logger = newDiscardLogger() // hclog.Logger — required by raft v1.7+

		logStore := raft.NewInmemStore()
		stableStore := raft.NewInmemStore()
		snapStore := raft.NewInmemSnapshotStore()

		r, err := raft.NewRaft(raftCfg, stateMachine, logStore, stableStore, snapStore, transports[i])
		if err != nil {
			t.Fatal(err)
		}

		// Bootstrap cluster on the first node only.
		if i == 0 {
			servers := make([]raft.Server, n)
			for j, p := range peers {
				servers[j] = raft.Server{
					ID:      raft.ServerID(p.ID),
					Address: raft.ServerAddress(p.RaftAddr),
				}
			}
			future := r.BootstrapCluster(raft.Configuration{Servers: servers})
			if err := future.Error(); err != nil && err != raft.ErrCantBootstrap {
				t.Fatal(err)
			}
		}

		nodeID := peers[i].ID

		// RaftNode — HTTPAddr will be back-filled in Phase 2.
		rn := &cluster.RaftNode{
			Raft:     r,
			NodeID:   nodeID,
			HTTPAddr: "", // set in Phase 2
		}

		// Start the HTTP test server. httpAddrs is passed by reference (it is
		// a map) so handlers will see the populated values after Phase 2.
		mux := http.NewServeMux()
		server.New(mux, rn, kvStore, peers, httpAddrs)
		httpSrv := httptest.NewServer(mux)
		t.Cleanup(httpSrv.Close)

		nodes[i] = &testNode{
			raftNode:  rn,
			kv:        kvStore,
			sqliteLog: sqliteLog,
			dedupSt:   dd,
			httpSrv:   httpSrv,
			dataDir:   dir,
		}
	}

	// ── Phase 2: populate httpAddrs with real server URLs ─────────────────────
	// httptest.Server.URL is only available after NewServer() returns, so we
	// must do this after Phase 1. Because Go maps are reference types, every
	// node's HTTP handler already holds a reference to this same map and will
	// see the updated values immediately — no re-registration needed.
	//
	// httpSrv.URL has the form "http://127.0.0.1:PORT".
	// forwardToLeader() prepends "http://", so we store only the host:port part.
	for i, n := range nodes {
		nodeID := peers[i].ID
		// Strip the "http://" prefix — forwardToLeader adds it back.
		url := n.httpSrv.URL // e.g. "http://127.0.0.1:54321"
		hostPort := url[len("http://"):]
		httpAddrs[nodeID] = hostPort
		n.raftNode.HTTPAddr = hostPort
	}

	t.Cleanup(func() {
		for _, n := range nodes {
			n.raftNode.Raft.Shutdown()
		}
	})

	return nodes
}

// waitForLeader blocks until exactly one leader is elected, or fails after timeout.
func waitForLeader(t *testing.T, nodes []*testNode, timeout time.Duration) *testNode {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			if n.raftNode.IsLeader() {
				return n
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no leader elected within timeout")
	return nil
}

// doPut sends a PUT request to the given node's HTTP server.
func doPut(t *testing.T, node *testNode, key, value, idempKey string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"key":             key,
		"value":           value,
		"idempotency_key": idempKey,
	})
	resp, err := http.Post(node.httpSrv.URL+"/put", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("PUT request failed: %v", err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
	return resp.StatusCode
}

// doGet sends a GET request and returns the value string.
func doGet(t *testing.T, node *testNode, key string) (string, int) {
	t.Helper()
	resp, err := http.Get(node.httpSrv.URL + "/get?key=" + key)
	if err != nil {
		t.Fatalf("GET request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode
	}
	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	return result["value"], resp.StatusCode
}

// newDiscardLogger returns an hclog.Logger that discards all output.
// hashicorp/raft v1.7+ requires hclog.Logger, not *log.Logger.
func newDiscardLogger() hclog.Logger {
	return hclog.NewNullLogger()
}

// ── Test 7.1.1: Election test ─────────────────────────────────────────────────

// TestElection verifies that exactly one leader is elected within 2 seconds
// of cluster startup.
func TestElection(t *testing.T) {
	nodes := newTestCluster(t)

	leader := waitForLeader(t, nodes, 2*time.Second)
	if leader == nil {
		t.Fatal("no leader elected")
	}

	// Count leaders — must be exactly 1.
	leaderCount := 0
	for _, n := range nodes {
		if n.raftNode.IsLeader() {
			leaderCount++
		}
	}
	if leaderCount != 1 {
		t.Fatalf("expected exactly 1 leader, got %d", leaderCount)
	}
	t.Logf("Leader elected: %s", leader.raftNode.NodeID)
}

// ── Test 7.1.2: Follower forward test ─────────────────────────────────────────

// TestFollowerForward sends a write to a FOLLOWER and verifies the entry is
// visible on all 3 nodes after forwarding reaches the leader.
func TestFollowerForward(t *testing.T) {
	nodes := newTestCluster(t)
	leader := waitForLeader(t, nodes, 2*time.Second)

	// Find a follower (not the leader).
	var follower *testNode
	for _, n := range nodes {
		if n != leader {
			follower = n
			break
		}
	}
	if follower == nil {
		t.Fatal("could not find a follower node")
	}

	// Send the write to the follower.
	status := doPut(t, follower, "hello", "world", "idem-forward-1")
	if status != http.StatusOK {
		t.Fatalf("PUT via follower returned %d, want 200", status)
	}

	// Wait briefly for replication to propagate to all nodes.
	time.Sleep(200 * time.Millisecond)

	// Verify the value is readable on ALL 3 nodes.
	for i, n := range nodes {
		val, code := doGet(t, n, "hello")
		if code != http.StatusOK {
			t.Errorf("node%d: GET returned %d", i+1, code)
			continue
		}
		if val != "world" {
			t.Errorf("node%d: expected value 'world', got '%s'", i+1, val)
		}
	}
	t.Log("Follower forwarding: OK — entry visible on all 3 nodes")
}

// ── Test 7.1.3: De-duplication test ───────────────────────────────────────────

// TestDedup sends the SAME idempotency_key twice and verifies the state machine
// only applies the command once.
func TestDedup(t *testing.T) {
	nodes := newTestCluster(t)
	leader := waitForLeader(t, nodes, 2*time.Second)

	const idempKey = "idem-dedup-unique-key-abc123"

	// First write: should succeed and set key = "v1"
	if s := doPut(t, leader, "counter", "v1", idempKey); s != http.StatusOK {
		t.Fatalf("first PUT returned %d", s)
	}

	// Give it time to commit and replicate.
	time.Sleep(150 * time.Millisecond)

	// Second write with SAME idempotency key: should be silently ignored.
	// The value should remain "v1", not change to "v2".
	if s := doPut(t, leader, "counter", "v2", idempKey); s != http.StatusOK {
		t.Fatalf("second PUT (duplicate) returned %d", s)
	}

	time.Sleep(150 * time.Millisecond)

	// Value must still be "v1" — the duplicate was discarded.
	val, code := doGet(t, leader, "counter")
	if code != http.StatusOK {
		t.Fatalf("GET returned %d", code)
	}
	if val != "v1" {
		t.Errorf("expected 'v1' after duplicate write, got '%s' — dedup failed!", val)
	}

	// The dedup store on the leader should have exactly 1 entry for this key.
	if leader.dedupSt.Len() < 1 {
		t.Error("dedup store should have at least 1 entry")
	}

	t.Log("De-duplication: OK — duplicate write correctly ignored")
}

// ── Test 7.2: Load test (optional) ────────────────────────────────────────────

// TestLoadThroughput sends 5,000 sequential writes and prints throughput.
// This is intentionally sequential (not concurrent) to measure baseline.
// You can increase concurrency for higher throughput.
func TestLoadThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	nodes := newTestCluster(t)
	leader := waitForLeader(t, nodes, 2*time.Second)

	const totalEntries = 5000
	var applied int64

	start := time.Now()

	for i := 0; i < totalEntries; i++ {
		key := fmt.Sprintf("load-key-%d", i)
		val := fmt.Sprintf("load-val-%d", i)
		idem := fmt.Sprintf("load-idem-%d", i)

		status := doPut(t, leader, key, val, idem)
		if status == http.StatusOK {
			atomic.AddInt64(&applied, 1)
		}
	}

	elapsed := time.Since(start)
	eps := float64(applied) / elapsed.Seconds()

	t.Logf("─────────────────────────────────────────────")
	t.Logf("Load test results:")
	t.Logf("  Total entries:  %d", totalEntries)
	t.Logf("  Applied:        %d", applied)
	t.Logf("  Duration:       %v", elapsed.Round(time.Millisecond))
	t.Logf("  Throughput:     %.0f entries/sec", eps)
	t.Logf("─────────────────────────────────────────────")

	if applied < totalEntries {
		t.Errorf("only %d/%d entries applied successfully", applied, totalEntries)
	}
}
