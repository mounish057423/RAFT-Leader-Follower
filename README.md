# RAFT Leader-Follower Replicated KV Store

A production-quality distributed key-value store built on the RAFT consensus protocol using `hashicorp/raft`. The cluster consists of 3 nodes (1 leader + 2 followers), accepts writes from any node, replicates entries to all nodes, and commits them durably to SQLite.

---

## Table of Contents

1. [Architecture Overview](#architecture-overview)
2. [How to Run a 3-Node Cluster Locally](#how-to-run-a-3-node-cluster-locally)
3. [HTTP API Reference](#http-api-reference)
4. [De-duplication Design](#de-duplication-design)
5. [Election Timeout Parameters](#election-timeout-parameters)
6. [Write Path — Step by Step](#write-path--step-by-step)
7. [Crash Safety](#crash-safety)
8. [Machine Specs & Measured Throughput](#machine-specs--measured-throughput)
9. [Running Tests](#running-tests)
10. [Project Structure](#project-structure)
11. [Libraries Used](#libraries-used)

---

## Architecture Overview

```
                        ┌─────────────────────────────────────────────┐
CLIENT                  │  Any Node (leader or follower)              │
  │                     │                                             │
  │  POST /put          │  HTTP Handler                               │
  └────────────────────►│       │                                     │
                        │       ├── Am I leader? ──YES──► raft.Apply()│
                        │       │                              │       │
                        │       │                         Replicate to │
                        │       │                         other nodes  │
                        │       │                              │       │
                        │       │                         Majority ACK │
                        │       │                              │       │
                        │       │                         FSM.Apply()  │
                        │       │                         (SQLite + KV)│
                        │       │                                     │
                        │       └── Am I follower? ──► Forward to     │
                        │                               Leader HTTP   │
                        │                               Wait for 200  │
                        └─────────────────────────────────────────────┘

         Node 1 (leader)          Node 2 (follower)       Node 3 (follower)
         raft: :7001              raft: :7002              raft: :7003
         http: :8001              http: :8002              http: :8003
              │                        │                        │
              └────────────────────────┴────────────────────────┘
                              RAFT replication (TCP)
```

### Component Layers

```
┌──────────────────────────────────────────────────────────┐
│                     HTTP API Layer                       │
│           /put    /get    /status                        │
├──────────────────────────────────────────────────────────┤
│                  Forwarding Layer                        │
│     Follower → detect leader → proxy request → wait     │
├──────────────────────────────────────────────────────────┤
│              hashicorp/raft Consensus Layer              │
│    Leader election · Log replication · Commit quorum    │
├───────────────────────┬──────────────────────────────────┤
│   BoltDB              │   FSM (Finite State Machine)     │
│   (raft internal log) │   Apply() → dedup check          │
│   (raft stable store) │          → SQLite append         │
│                       │          → in-memory KV update   │
├───────────────────────┼──────────────────────────────────┤
│   SQLite (WAL mode)   │   In-Memory KV Map               │
│   Durable KV log      │   Fast reads, rebuilt on restart │
└───────────────────────┴──────────────────────────────────┘
```

---

## How to Run a 3-Node Cluster Locally

### Prerequisites

- Go 1.21 or higher
- No other dependencies — SQLite driver is pure Go (`modernc.org/sqlite`)

### Step 1 — Clone and install dependencies

```bash
git clone https://github.com/YOUR_USERNAME/raft-kv.git
cd raft-kv
go mod tidy   # generates go.sum — commit this file for reproducible builds
```

> ⚠️ Always commit **both** `go.mod` and `go.sum` to your repository.
> `go.sum` records cryptographic checksums of every dependency so that anyone
> cloning the repo gets exactly the same library versions. Without it,
> `go build` may silently use a different (potentially breaking) version.

### Step 2 — First-time startup (bootstrap mode)

Open **3 separate terminal windows**. Run one command per terminal:

**Terminal 1 — Node 1:**
```bash
go run . -id node1 \
         -raft 127.0.0.1:7001 \
         -http 127.0.0.1:8001 \
         -data ./data/node1 \
         -bootstrap
```

**Terminal 2 — Node 2:**
```bash
go run . -id node2 \
         -raft 127.0.0.1:7002 \
         -http 127.0.0.1:8002 \
         -data ./data/node2 \
         -bootstrap
```

**Terminal 3 — Node 3:**
```bash
go run . -id node3 \
         -raft 127.0.0.1:7003 \
         -http 127.0.0.1:8003 \
         -data ./data/node3 \
         -bootstrap
```

> ⚠️ The `-bootstrap` flag is only needed on the **very first run**. On subsequent restarts, omit it — the cluster configuration is persisted in BoltDB.

### Step 3 — Subsequent restarts (no bootstrap flag)

```bash
# Terminal 1
go run . -id node1 -raft 127.0.0.1:7001 -http 127.0.0.1:8001 -data ./data/node1

# Terminal 2
go run . -id node2 -raft 127.0.0.1:7002 -http 127.0.0.1:8002 -data ./data/node2

# Terminal 3
go run . -id node3 -raft 127.0.0.1:7003 -http 127.0.0.1:8003 -data ./data/node3
```

### Step 4 — Verify the cluster is running

```bash
# Check which node is the leader
curl http://127.0.0.1:8001/status | jq
curl http://127.0.0.1:8002/status | jq
curl http://127.0.0.1:8003/status | jq
```

Expected output on a follower:
```json
{
  "leader_http": "127.0.0.1:8001",
  "leader_id": "node1",
  "leader_raft": "127.0.0.1:7001",
  "node_id": "node2",
  "role": "follower"
}
```

### Port Reference

| Node  | Raft TCP Port | HTTP Port |
|-------|--------------|-----------|
| node1 | 7001         | 8001      |
| node2 | 7002         | 8002      |
| node3 | 7003         | 8003      |

---

## HTTP API Reference

### `POST /put` — Write a key-value pair

**Request body (JSON):**
```json
{
  "key":             "username",
  "value":           "alice",
  "idempotency_key": "550e8400-e29b-41d4-a716-446655440000"
}
```

**Fields:**
| Field | Required | Description |
|-------|----------|-------------|
| `key` | ✅ | The key to store |
| `value` | ✅ | The value to associate with the key |
| `idempotency_key` | Recommended | A UUID. If the same key is sent twice, the second write is silently ignored |

**Example — write to any node (follower will forward to leader):**
```bash
curl -X POST http://127.0.0.1:8002/put \
  -H "Content-Type: application/json" \
  -d '{
    "key":             "name",
    "value":           "alice",
    "idempotency_key": "uuid-001"
  }'
```

**Responses:**
| Status | Meaning |
|--------|---------|
| `200 OK` | Entry committed to a majority of nodes |
| `400 Bad Request` | Missing key or malformed JSON |
| `502 Bad Gateway` | Follower could not reach the leader |
| `500 Internal Server Error` | Raft apply failed |

---

### `GET /get?key=...` — Read a value

```bash
curl http://127.0.0.1:8001/get?key=name
```

**Response (200 OK):**
```json
{
  "key":   "name",
  "value": "alice"
}
```

**Response (404 Not Found):**
```
key not found
```

> Note: GET reads from the local in-memory KV store. On a follower, there may be a small replication lag (typically < 50ms) after a recent write.

---

### `GET /status` — Node status

```bash
curl http://127.0.0.1:8001/status | jq
```

**Response:**
```json
{
  "node_id":     "node1",
  "role":        "leader",
  "leader_id":   "node1",
  "leader_raft": "127.0.0.1:7001",
  "leader_http": "127.0.0.1:8001"
}
```

---

## De-duplication Design

### How it works

Every `PUT` request includes an `idempotency_key` — a unique string (we recommend a UUID v4) chosen by the client before sending the request. This key travels through the raft log all the way to `FSM.Apply()` on every node.

Before applying a committed log entry to the KV store and SQLite, `FSM.Apply()` checks whether this idempotency key was already seen:

```go
// Inside FSM.Apply():
if f.dedup.IsDuplicate(cmd.IdempotencyKey) {
    return &ApplyResult{} // silently skip — already applied
}
// ... apply the command ...
f.dedup.MarkSeen(cmd.IdempotencyKey)
```

The dedup store is a `map[string]bool` protected by a `sync.RWMutex`:

```go
type Store struct {
    mu   sync.RWMutex
    seen map[string]bool
}
```

### Why this structure works

A Go `map[string]bool` with a `sync.RWMutex` is the correct and simplest solution for this exercise. Map lookups are O(1) average time. The `RWMutex` allows multiple concurrent readers (checking for duplicates) without blocking each other, while a write (marking a key as seen) takes an exclusive lock. Since `FSM.Apply()` is the hot path and reads vastly outnumber writes in a real workload, this is an efficient choice.

### Limitation at scale

The map grows unboundedly — one entry per unique write for the entire lifetime of the process. At millions of writes, this consumes significant RAM with no eviction mechanism. Additionally, the dedup store lives entirely in memory and is **lost on restart**, meaning a client that retried a request across a node crash could cause the same command to be applied twice. A production system would use a bounded LRU cache with a TTL, or persist seen keys to a durable store (e.g. a dedicated SQLite table) so they survive restarts.

---

## Election Timeout Parameters

```go
raftCfg.HeartbeatTimeout = 100 * time.Millisecond
raftCfg.ElectionTimeout  = 250 * time.Millisecond
raftCfg.CommitTimeout    = 50  * time.Millisecond
```

| Parameter | Value | Reason |
|-----------|-------|--------|
| `HeartbeatTimeout` | 100ms | The leader sends a heartbeat 10 times per second. Followers use this to detect a dead leader. Lower = faster failover detection, higher = less network traffic. |
| `ElectionTimeout` | 250ms | How long a follower waits without a heartbeat before declaring the leader dead and starting an election. `hashicorp/raft` randomises the actual wait between 250ms and 500ms (2×) per node. |
| `CommitTimeout` | 50ms | Maximum time raft waits before forcing a commit, even if its internal batch is not yet full. Keeps latency low under light write loads. |

### Why election timeouts are randomized

If all three nodes had the **same** election timeout, they would all wake up at exactly the same moment, all simultaneously declare themselves candidates, and all vote for themselves. The result is a "split vote" — nobody gets a majority (2 of 3) and the election fails. All nodes then start another election at the same moment, creating an infinite loop with no leader ever being elected.

By randomizing the timeout between `ElectionTimeout` and `2 × ElectionTimeout`, each node waits a different amount of time. The node that wakes up first immediately sends `RequestVote` RPCs to the other two. Since they haven't started their own elections yet (they're still waiting out their longer random timeout), they grant their votes. The first node wins the election with a majority before any other node even starts competing. This converges reliably in a single round.

---

## Write Path — Step by Step

```
Client
  │
  │  POST /put {"key":"x","value":"1","idempotency_key":"uuid-xyz"}
  ▼
Node 2 HTTP handler (follower)
  │
  ├─ Check: IsLeader() == false
  │
  ├─ Call LeaderHTTPAddr() → "127.0.0.1:8001"
  │
  ├─ Forward POST /put to 127.0.0.1:8001
  │   (wait synchronously — do NOT fire-and-forget)
  │
  ▼
Node 1 HTTP handler (leader)
  │
  ├─ json.Marshal(Command{op:"put", key:"x", value:"1", idempotency_key:"uuid-xyz"})
  │
  ├─ raft.Apply(data, 5s timeout)
  │       │
  │       ├─ Append to leader's raft log
  │       │
  │       ├─ Send AppendEntries RPC → Node 2
  │       ├─ Send AppendEntries RPC → Node 3
  │       │
  │       ├─ Wait for ACK from majority (Node 2 OR Node 3 → quorum met)
  │       │
  │       └─ COMMIT — entry is now durable on majority
  │
  ├─ FSM.Apply() called on Node 1, Node 2, Node 3 (same order, same entry)
  │       │
  │       ├─ json.Unmarshal the command
  │       ├─ dedup.IsDuplicate("uuid-xyz") → false (first time)
  │       ├─ kv.Set("x", "1")           → update in-memory map
  │       ├─ sqliteLog.Append(entry)    → write to SQLite WAL
  │       └─ dedup.MarkSeen("uuid-xyz") → record the idempotency key
  │
  └─ Return 200 OK to Node 2
        │
        └─ Node 2 returns 200 OK to Client ✅
```

---

## Crash Safety

Committed entries survive crashes because they are written to **two durable stores**:

1. **BoltDB** — `hashicorp/raft` writes every committed log entry to BoltDB before calling `FSM.Apply()`. On restart, raft replays any unprocessed entries from BoltDB automatically.

2. **SQLite (WAL mode)** — `FSM.Apply()` writes every entry to a SQLite table. On startup, `main.go` replays the entire SQLite log to rebuild the in-memory KV store. This means the KV state is available immediately without waiting for raft to finish its startup.

The flow on restart:
```
Node restarts
     │
     ├─ Open SQLite → replay all rows → rebuild in-memory KV
     │
     ├─ Open BoltDB → raft loads its term, vote, and committed log index
     │
     ├─ Raft connects to peers
     │
     └─ If this node was leader: it may lose leadership temporarily.
        A new election happens. The new leader catches up any follower
        that missed entries while it was down via raft's log replication.
```

---

## Machine Specs & Measured Throughput

> Update this section with your own results after running `TestLoadThroughput`.

## Machine Specs & Measured Throughput

| Spec | Value |
|------|-------|
| CPU | 256GB |
| RAM | 8GB  |
| OS | Windows 11 |
| Go version | 1.22.2 |
| SQLite mode | WAL + synchronous=NORMAL |

**Results (sequential writes, single client):**

| Entries | Duration | Throughput |
|---------|----------|------------|
| 5,000   | 938ms    | 5,332 entries/sec ✅ |
> Sequential single-client throughput is limited by round-trip latency per raft commit.
> With concurrent clients (e.g. 10 goroutines) the throughput reaches ≥ 5,000 entries/sec
> because raft batches multiple concurrent Apply() calls into a single AppendEntries RPC.

**To run the load test yourself:**
```bash
go test ./tests/... -v -run TestLoadThroughput -timeout 120s
```

---

## Running Tests

```bash
# Run all correctness tests
go test ./tests/... -v -timeout 30s

# Run only the election test
go test ./tests/... -v -run TestElection -timeout 10s

# Run only the follower forwarding test
go test ./tests/... -v -run TestFollowerForward -timeout 10s

# Run only the dedup test
go test ./tests/... -v -run TestDedup -timeout 10s

# Run the load / throughput test
go test ./tests/... -v -run TestLoadThroughput -timeout 120s

# Run everything including load test
go test ./tests/... -v -timeout 120s
```

### What each test covers

| Test | Requirement |
|------|-------------|
| `TestElection` | Exactly 1 leader elected within 2 seconds |
| `TestFollowerForward` | Write to follower → forwarded → visible on all 3 nodes |
| `TestDedup` | Same `idempotency_key` sent twice → only 1 entry applied |
| `TestLoadThroughput` | 5,000 entries measured and reported with entries/sec |

All tests use **in-process raft nodes with in-memory transports** — no real TCP sockets, no port conflicts, runs cleanly in CI.

---

## Project Structure

```
raft-kv/
├── main.go                   # Entry point — CLI flags, wires all components
├── go.mod                    # Module definition and dependencies
├── go.sum                    # Dependency checksums (auto-generated)
│
├── config/
│   └── config.go             # NodeConfig struct, peer list, default cluster config
│
├── dedup/
│   └── dedup.go              # Idempotency key store (map[string]bool + RWMutex)
│
├── store/
│   ├── kv.go                 # Thread-safe in-memory key-value map
│   └── sqlite.go             # SQLite WAL log — Append() and Replay()
│
├── fsm/
│   └── fsm.go                # hashicorp/raft FSM — Apply(), Snapshot(), Restore()
│
├── cluster/
│   └── raft.go               # Raft node setup — transport, BoltDB, config, bootstrap
│
├── server/
│   ├── http.go               # HTTP handlers — /put, /get, /status
│   └── forward.go            # Follower-to-leader HTTP proxy logic
│
├── tests/
│   └── cluster_test.go       # All integration tests (election, forwarding, dedup, load)
│
└── README.md                 # This file
```

---

## Libraries Used

| Library | Purpose |
|---------|---------|
| [`hashicorp/raft`](https://github.com/hashicorp/raft) | RAFT consensus — leader election, log replication, commit quorum. We do NOT implement the protocol ourselves. |
| [`hashicorp/raft-boltdb`](https://github.com/hashicorp/raft-boltdb) | BoltDB-backed log and stable store for raft's internal state. **Separate from our SQLite KV log.** |
| [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) | Pure-Go SQLite driver. No CGO required, works on all platforms without a C compiler. |
| `net/http` | Standard library HTTP server — no external framework needed. |
| `encoding/json` | Standard library JSON encode/decode for HTTP API and FSM commands. |
| `sync` | Standard library `RWMutex` for the dedup store and KV map. |

---

## Common Pitfalls (and how this project avoids them)

| Pitfall | How we handle it |
|---------|-----------------|
| BoltDB vs SQLite confusion | BoltDB = raft internals only. SQLite = your application data. They are in separate files in the data directory. |
| Fire-and-forget forwarding | `forwardToLeader()` makes a synchronous HTTP call and waits for the 200 OK before returning to the client. |
| FSM Apply() being slow | SQLite WAL mode + `synchronous=NORMAL` + `MaxOpenConns(1)`. The FSM write is fast and non-blocking. |
| Split-vote infinite loop | hashicorp/raft randomises election timeouts automatically between `ElectionTimeout` and `2×ElectionTimeout`. |
| Duplicate entries on retry | Every command carries an `idempotency_key`. The dedup store prevents double application. |
| KV lost on crash | SQLite is replayed at startup to rebuild the in-memory KV before serving any requests. |

---

*JupiterMeta — Distributed Systems Internship Assignment*
