/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command example-mcp is a minimal Model Context Protocol server used to verify
// CogNet end-to-end. It speaks MCP's JSON-RPC 2.0 over HTTP POST (a valid
// Streamable-HTTP request/response), implementing initialize, tools/list, and
// tools/call for a single demo tool: get_order_status(orderId).
//
// It is a test fixture, not production MCP — but it is a real MCP wire peer:
// the agent runtime talks to it exactly as it would any MCP tool server.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const orderToolSchema = `{
  "type": "object",
  "properties": { "orderId": { "type": "string", "description": "the order id to look up" } },
  "required": ["orderId"]
}`

const weatherToolSchema = `{
  "type": "object",
  "properties": { "city": { "type": "string", "description": "city name, e.g. Beijing or Shanghai" } },
  "required": ["city"]
}`

func main() {
	var addr string
	flag.StringVar(&addr, "addr", ":8080", "listen address")
	flag.Parse()
	if v := os.Getenv("PORT"); v != "" {
		addr = ":" + v
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/", handleRPC)

	log.Printf("example-mcp listening on %s", addr)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, nil, -32700, "parse error")
		return
	}

	// Notifications (no id) get an empty 202.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		writeResult(w, req.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "cognet-example-mcp", "version": "0.1.0"},
		})
	case "tools/list":
		writeResult(w, req.ID, map[string]any{
			"tools": []map[string]any{
				{
					"name":        "get_order_status",
					"description": "Look up the delivery status of a customer order by its order id.",
					"inputSchema": json.RawMessage(orderToolSchema),
				},
				{
					"name":        "get_weather",
					"description": "Get the current weather for a city.",
					"inputSchema": json.RawMessage(weatherToolSchema),
				},
			},
		})
	case "tools/call":
		handleToolCall(w, &req)
	default:
		writeErr(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

func handleToolCall(w http.ResponseWriter, req *rpcRequest) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(req.Params, &params)
	switch params.Name {
	case "get_order_status":
		handleGetOrderStatus(w, req, params.Arguments)
	case "get_weather":
		handleGetWeather(w, req, params.Arguments)
	default:
		writeErr(w, req.ID, -32602, "unknown tool: "+params.Name)
	}
}

func handleGetWeather(w http.ResponseWriter, req *rpcRequest, args map[string]any) {
	city, _ := args["city"].(string)
	if city == "" {
		city = "unknown"
	}
	text := fetchWeather(city)
	writeResult(w, req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": false,
	})
}

// fetchWeather queries wttr.in (free, no API key) for a concise current
// conditions line. Falls back to a canned message on error so the demo never
// hard-fails offline.
func fetchWeather(city string) string {
	u := "https://wttr.in/" + url.PathEscape(city) + "?format=%l:+%C,+%t,+humidity+%h,+wind+%w&m"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return fmt.Sprintf("weather for %s unavailable", city)
	}
	req.Header.Set("User-Agent", "curl/8")
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return fmt.Sprintf("weather for %s unavailable (%v)", city, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || len(body) == 0 {
		return fmt.Sprintf("weather for %s unavailable", city)
	}
	return string(body)
}

func handleGetOrderStatus(w http.ResponseWriter, req *rpcRequest, args map[string]any) {
	orderID, _ := args["orderId"].(string)
	if orderID == "" {
		orderID = "unknown"
	}
	// Canned but deterministic result.
	result := map[string]any{
		"orderId": orderID,
		"status":  "shipped",
		"carrier": "UPS",
		"eta":     "2026-07-20",
	}
	payload, _ := json.Marshal(result)
	writeResult(w, req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(payload)}},
		"isError": false,
	})
}

func writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeErr(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}
