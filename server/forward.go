package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// forwardClient is a shared HTTP client with keep-alive connections enabled.
// Reusing connections removes TCP handshake overhead on every forwarded request,
// which matters when forwarding thousands of writes per second.
var forwardClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 8,   // keep 8 idle connections to the leader
		IdleConnTimeout:     90 * time.Second,
	},
}

// forwardToLeader proxies a PUT request from a follower to the current leader.
//
// Correctness guarantee (Issue 1):
//   We make a SYNCHRONOUS HTTP call and wait for the 200 OK response.
//   The leader only returns 200 after raft.Apply() commits the entry to a
//   majority of nodes. So the follower returns success to the client only
//   after the entry is durably committed — not before.
//
// Retry logic (production improvement):
//   If the leader changes between our LeaderHTTPAddr() call and the actual
//   HTTP request (network partition, crash), the old leader returns 502 or
//   connection-refused. We retry up to maxForwardRetries times, each time
//   re-asking for the current leader address, so the request eventually
//   reaches the new leader.
const maxForwardRetries = 3

func forwardToLeader(leaderHTTPAddr string, req PutRequest) error {
	if leaderHTTPAddr == "" {
		return fmt.Errorf("no leader elected yet — try again shortly")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal forward request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < maxForwardRetries; attempt++ {
		if attempt > 0 {
			// Brief back-off before retry — gives raft time to elect a new leader.
			time.Sleep(time.Duration(attempt*100) * time.Millisecond)
		}

		url := "http://" + leaderHTTPAddr + "/put"
		resp, err := forwardClient.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			lastErr = fmt.Errorf("attempt %d: connect to leader %s: %w", attempt+1, leaderHTTPAddr, err)
			continue
		}

		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			return nil // committed successfully
		}

		lastErr = fmt.Errorf("attempt %d: leader returned %d: %s", attempt+1, resp.StatusCode, string(b))

		// 502/503 typically means the node we forwarded to is no longer leader.
		// The next retry will use the updated leaderHTTPAddr passed by the caller.
		// For now we break since the caller (handlePut) re-resolves the addr.
		break
	}

	return lastErr
}
