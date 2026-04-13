package config

// NodeConfig holds all configuration for a single raft node.
type NodeConfig struct {
	// NodeID is the unique identifier for this node (e.g. "node1", "node2", "node3")
	NodeID string

	// RaftAddr is the TCP address this node uses for raft peer-to-peer communication
	// e.g. "127.0.0.1:7000"
	RaftAddr string

	// HTTPAddr is the address this node listens on for client HTTP requests
	// e.g. "127.0.0.1:8000"
	HTTPAddr string

	// DataDir is the directory where raft's BoltDB log and SQLite file are stored
	// e.g. "./data/node1"
	DataDir string

	// Peers is the list of all nodes in the cluster (including this node itself)
	Peers []Peer

	// Bootstrap should be true ONLY for the very first startup of the cluster.
	// It tells raft: "I am the first node, go ahead and form a cluster."
	Bootstrap bool
}

// Peer represents one node in the cluster from another node's perspective.
type Peer struct {
	ID       string // matches NodeID of that node
	RaftAddr string // raft TCP address of that node
}

// DefaultClusterPeers returns the standard 3-node peer list used in local testing.
func DefaultClusterPeers() []Peer {
	return []Peer{
		{ID: "node1", RaftAddr: "127.0.0.1:7001"},
		{ID: "node2", RaftAddr: "127.0.0.1:7002"},
		{ID: "node3", RaftAddr: "127.0.0.1:7003"},
	}
}
