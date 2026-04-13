// Package cluster handles all hashicorp/raft setup and wiring.
//
// What does this package do?
// --------------------------
// hashicorp/raft needs several components wired together before it can run:
//
//  1. Transport  — how nodes talk to each other (TCP in our case)
//  2. LogStore   — where raft stores its own internal log (BoltDB)
//  3. StableStore— where raft stores its current term and voted-for (also BoltDB)
//  4. SnapshotStore — where raft stores point-in-time snapshots
//  5. FSM        — YOUR code that applies committed entries (see fsm package)
//  6. Config     — timeouts, node IDs, etc.
//
// This package assembles all of those and returns a running *raft.Raft instance.
package cluster

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"raft-kv/config"
)

// RaftNode wraps the hashicorp/raft instance and exposes helpers used by
// the HTTP server (forwarding writes, querying state).
type RaftNode struct {
	Raft     *raft.Raft
	NodeID   string
	HTTPAddr string // this node's HTTP address — needed for /status
}

// New creates and starts a raft node according to cfg.
// fsm must implement the raft.FSM interface.
func New(cfg *config.NodeConfig, fsm raft.FSM) (*RaftNode, error) {
	// ── 1. Build the raft configuration ──────────────────────────────────────
	raftCfg := raft.DefaultConfig()
	raftCfg.LocalID = raft.ServerID(cfg.NodeID)

	// Heartbeat: how often the leader pings followers to say "I'm alive."
	// 100 ms means followers hear from the leader ~10 times per second.
	raftCfg.HeartbeatTimeout = 500 * time.Millisecond

	// Election timeout: how long a follower waits WITHOUT a heartbeat before
	// it decides the leader is dead and starts a new election.
	// hashicorp/raft randomises the actual value between ElectionTimeout and
	// 2×ElectionTimeout automatically — this prevents split votes where all
	// followers start an election at the exact same millisecond.
	raftCfg.ElectionTimeout = 1000 * time.Millisecond

	// CommitTimeout: max time raft will wait before flushing a partial batch.
	// Keeps latency low when write rate is low.
	raftCfg.LeaderLeaseTimeout = 500 * time.Millisecond
	raftCfg.CommitTimeout = 50 * time.Millisecond



	// MaxAppendEntries: maximum number of log entries sent in one AppendEntries
	// RPC. Higher values let raft batch more work per network round-trip.
	// Default is 64; 256 improves throughput under high write load.
	raftCfg.MaxAppendEntries = 256

	// ── 2. TCP transport (how raft peers talk to each other) ─────────────────
	addr, err := net.ResolveTCPAddr("tcp", cfg.RaftAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve raft addr %s: %w", cfg.RaftAddr, err)
	}

	transport, err := raft.NewTCPTransport(cfg.RaftAddr, addr, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("create TCP transport: %w", err)
	}

	// ── 3. Create data directory ──────────────────────────────────────────────
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	// ── 4. BoltDB log + stable store ─────────────────────────────────────────
	// BoltDB is a key-value database embedded in a single file.
	// hashicorp/raft uses it to durably store:
	//   - LogStore:    the raft log entries (separate from your SQLite!)
	//   - StableStore: current term + who this node voted for
	// These are INTERNAL to raft — you never read/write them directly.
	boltPath := filepath.Join(cfg.DataDir, "raft.db")
	boltStore, err := raftboltdb.NewBoltStore(boltPath)
	if err != nil {
		return nil, fmt.Errorf("create boltdb store: %w", err)
	}

	// ── 5. Snapshot store ─────────────────────────────────────────────────────
	// Snapshots allow raft to truncate old log entries after a point-in-time
	// state has been captured. Stored as files in the data directory.
	snapStore, err := raft.NewFileSnapshotStore(cfg.DataDir, 2, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("create snapshot store: %w", err)
	}

	// ── 6. Create the raft instance ───────────────────────────────────────────
	r, err := raft.NewRaft(raftCfg, fsm, boltStore, boltStore, snapStore, transport)
	if err != nil {
		return nil, fmt.Errorf("create raft: %w", err)
	}

	// ── 7. Bootstrap the cluster (first-time only) ────────────────────────────
	// Bootstrap tells raft "here are all the members of the cluster."
	// We only do this when cfg.Bootstrap == true (typically for node1 only,
	// or for ALL nodes on a fresh first-start).
	// On subsequent restarts the cluster state is loaded from BoltDB — no
	// bootstrap needed.
	if cfg.Bootstrap {
		servers := make([]raft.Server, len(cfg.Peers))
		for i, p := range cfg.Peers {
			servers[i] = raft.Server{
				ID:      raft.ServerID(p.ID),
				Address: raft.ServerAddress(p.RaftAddr),
			}
		}
		raftCfg := raft.Configuration{Servers: servers}
		future := r.BootstrapCluster(raftCfg)
		// BootstrapCluster returns a future whose error must be checked.
		// ErrCantBootstrap means the cluster was already bootstrapped (BoltDB
		// already has cluster config) — that is expected and safe on restarts.
		// Any other error is a real problem (e.g. duplicate node IDs, bad addrs).
		if err := future.Error(); err != nil && err != raft.ErrCantBootstrap {
			return nil, fmt.Errorf("bootstrap cluster: %w", err)
		}
	}

	return &RaftNode{
		Raft:     r,
		NodeID:   cfg.NodeID,
		HTTPAddr: cfg.HTTPAddr,
	}, nil
}

// IsLeader returns true if this node currently believes it is the raft leader.
//
// Issue 7 fix — leader detection race:
// raft.State() can momentarily return Leader even after the node has lost
// leadership (e.g. during a network partition). We use LeaderWithID() which
// queries the stable leader address stored by raft consensus, and cross-check
// it against our own ID. If they match we are the leader.
// The Apply() method handles the residual race by catching ErrNotLeader.
func (rn *RaftNode) IsLeader() bool {
	_, leaderID := rn.Raft.LeaderWithID()
	return string(leaderID) == rn.NodeID
}

// LeaderHTTPAddr returns the HTTP address of the current leader.
// It does this by looking up the leader's raft address in the cluster
// configuration and mapping it to an HTTP address via the known peer list.
//
// Returns "" if there is no leader yet.
func (rn *RaftNode) LeaderHTTPAddr(peers []config.Peer, httpAddrs map[string]string) string {
	leaderAddr, leaderID := rn.Raft.LeaderWithID()
	if leaderAddr == "" {
		return ""
	}
	_ = leaderAddr // we use leaderID to look up the HTTP address
	return httpAddrs[string(leaderID)]
}

// Apply submits a command to the raft log and waits for it to be committed.
// Only call this on the LEADER node.
//
// Returns (true, nil) on success.
// Returns (false, nil) when this node lost leadership between the IsLeader()
// check and the Apply() call (ErrNotLeader). The caller should forward the
// request to the new leader instead of returning an error to the client.
// Returns (false, err) for all other errors.
func (rn *RaftNode) Apply(data []byte, timeout time.Duration) (bool, error) {
	future := rn.Raft.Apply(data, timeout)
	if err := future.Error(); err != nil {
		// ErrNotLeader means leadership changed between our IsLeader() check
		// and this Apply() call. Return false so the HTTP handler can forward
		// the request to whoever is now the leader.
		if err == raft.ErrNotLeader {
			return false, nil
		}
		return false, fmt.Errorf("raft apply: %w", err)
	}
	return true, nil
}

// Stats returns raft's internal stats map (useful for /status endpoint).
func (rn *RaftNode) Stats() map[string]string {
	return rn.Raft.Stats()
}
