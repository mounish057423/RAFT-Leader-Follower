package server

import (
	"net/http"
	"strings"
)

// swaggerSpec is the OpenAPI 3.0 specification for the raft-kv HTTP API.
// Written by hand — no code generation needed.
const swaggerSpec = `{
  "openapi": "3.0.0",
  "info": {
    "title": "raft-kv API",
    "description": "Distributed key-value store built on the Raft consensus protocol. Any node accepts reads and writes — followers auto-forward writes to the current leader.",
    "version": "1.0.0"
  },
  "servers": [
    { "url": "http://127.0.0.1:8001", "description": "node1" },
    { "url": "http://127.0.0.1:8002", "description": "node2" },
    { "url": "http://127.0.0.1:8003", "description": "node3" }
  ],
  "paths": {
    "/put": {
      "post": {
        "summary": "Write a key-value pair",
        "description": "Submits a PUT command to the Raft log. If this node is a follower it automatically forwards the request to the current leader and waits for majority commit before returning 200.",
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": { "$ref": "#/components/schemas/PutRequest" },
              "example": {
                "key": "name",
                "value": "alice",
                "idempotency_key": "550e8400-e29b-41d4-a716-446655440000"
              }
            }
          }
        },
        "responses": {
          "200": { "description": "Write committed to majority of nodes" },
          "400": { "description": "Missing key or idempotency_key" },
          "503": { "description": "Server overloaded or no leader elected yet" }
        }
      }
    },
    "/get": {
      "get": {
        "summary": "Read a value by key",
        "description": "Reads directly from this node's in-memory KV store. All nodes return the same value once replication has caught up.",
        "parameters": [
          {
            "name": "key",
            "in": "query",
            "required": true,
            "schema": { "type": "string" },
            "example": "name"
          }
        ],
        "responses": {
          "200": {
            "description": "Key found",
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/GetResponse" },
                "example": { "key": "name", "value": "alice" }
              }
            }
          },
          "404": { "description": "Key not found" }
        }
      }
    },
    "/status": {
      "get": {
        "summary": "Cluster status",
        "description": "Returns this node's role (leader or follower) and the current leader's addresses.",
        "responses": {
          "200": {
            "description": "Status info",
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/StatusResponse" },
                "example": {
                  "node_id": "node1",
                  "role": "follower",
                  "leader_id": "node2",
                  "leader_raft": "127.0.0.1:7002",
                  "leader_http": "127.0.0.1:8002"
                }
              }
            }
          }
        }
      }
    },
    "/health": {
      "get": {
        "summary": "Liveness check",
        "description": "Always returns 200 as long as the process is running. Use for load balancer health checks.",
        "responses": {
          "200": {
            "description": "Node is alive",
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/HealthResponse" },
                "example": { "status": "ok", "node_id": "node1", "role": "Leader" }
              }
            }
          }
        }
      }
    },
    "/metrics": {
      "get": {
        "summary": "Live metrics",
        "description": "Returns throughput, latency percentiles, error counts and queue depth for this node.",
        "responses": {
          "200": {
            "description": "Metrics snapshot",
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/MetricsResponse" },
                "example": {
                  "total_writes": 1234,
                  "total_duplicates": 2,
                  "total_errors": 0,
                  "total_forwards": 10,
                  "batch_queue_depth": 0,
                  "dedup_map_size": 1234,
                  "writes_per_sec": 487.3,
                  "latency_p50_ms": 1.8,
                  "latency_p95_ms": 4.2,
                  "latency_p99_ms": 9.1
                }
              }
            }
          }
        }
      }
    }
  },
  "components": {
    "schemas": {
      "PutRequest": {
        "type": "object",
        "required": ["key", "value", "idempotency_key"],
        "properties": {
          "key":             { "type": "string", "description": "The key to store" },
          "value":           { "type": "string", "description": "The value to associate with the key" },
          "idempotency_key": { "type": "string", "description": "A unique UUID. Sending the same key twice silently ignores the second write." }
        }
      },
      "GetResponse": {
        "type": "object",
        "properties": {
          "key":   { "type": "string" },
          "value": { "type": "string" }
        }
      },
      "StatusResponse": {
        "type": "object",
        "properties": {
          "node_id":     { "type": "string" },
          "role":        { "type": "string", "enum": ["leader", "follower"] },
          "leader_id":   { "type": "string" },
          "leader_raft": { "type": "string" },
          "leader_http": { "type": "string" }
        }
      },
      "HealthResponse": {
        "type": "object",
        "properties": {
          "status":  { "type": "string" },
          "node_id": { "type": "string" },
          "role":    { "type": "string" }
        }
      },
      "MetricsResponse": {
        "type": "object",
        "properties": {
          "total_writes":      { "type": "integer" },
          "total_duplicates":  { "type": "integer" },
          "total_errors":      { "type": "integer" },
          "total_forwards":    { "type": "integer" },
          "batch_queue_depth": { "type": "integer" },
          "dedup_map_size":    { "type": "integer" },
          "writes_per_sec":    { "type": "number" },
          "latency_p50_ms":    { "type": "number" },
          "latency_p95_ms":    { "type": "number" },
          "latency_p99_ms":    { "type": "number" }
        }
      }
    }
  }
}`

// swaggerUI returns the Swagger UI HTML page.
// It loads Swagger UI assets from the official CDN so no local files are needed.
// The spec is embedded inline so it works fully offline once the page is loaded.
func swaggerUI(specURL string) string {
	return `<!DOCTYPE html>
<html>
<head>
  <title>raft-kv API</title>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <link rel="stylesheet" type="text/css" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>
  SwaggerUIBundle({
    url: "` + specURL + `",
    dom_id: '#swagger-ui',
    presets: [SwaggerUIBundle.presets.apis, SwaggerUIBundle.SwaggerUIStandalonePreset],
    layout: "BaseLayout",
    deepLinking: true
  })
</script>
</body>
</html>`
}

// registerSwagger wires the Swagger UI and spec endpoints onto mux.
//
//	GET /swagger/      — Swagger UI (open in browser)
//	GET /swagger/spec  — raw OpenAPI JSON spec
func registerSwagger(mux *http.ServeMux) {
	// Serve the raw OpenAPI spec.
	mux.HandleFunc("/swagger/spec", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write([]byte(swaggerSpec))
	})

	// Serve the Swagger UI. Accept /swagger/ and /swagger (without trailing slash).
	uiHandler := func(w http.ResponseWriter, r *http.Request) {
		// Build the spec URL using the same host the browser used, so it works
		// on any port (node1=8001, node2=8002, node3=8003).
		host := r.Host
		if host == "" {
			host = "127.0.0.1:8001"
		}
		specURL := "http://" + host + "/swagger/spec"
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(swaggerUI(specURL)))
	}

	mux.HandleFunc("/swagger/", uiHandler)
	mux.HandleFunc("/swagger", func(w http.ResponseWriter, r *http.Request) {
		// Redirect /swagger → /swagger/ so relative assets resolve correctly.
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, "/swagger/", http.StatusMovedPermanently)
			return
		}
		uiHandler(w, r)
	})
}
