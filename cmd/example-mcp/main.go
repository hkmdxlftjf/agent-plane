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
// Agent Plane end-to-end. It speaks MCP's JSON-RPC 2.0 over HTTP POST (a valid
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

const attractionsToolSchema = `{
  "type": "object",
  "properties": {
    "city":       { "type": "string", "description": "city name, e.g. Beijing" },
    "preference": { "type": "string", "description": "traveler preference, e.g. history, nature, food" }
  },
  "required": ["city"]
}`

const hotelsToolSchema = `{
  "type": "object",
  "properties": {
    "city": { "type": "string", "description": "city name, e.g. Beijing" },
    "type": { "type": "string", "description": "accommodation type: budget | comfort | luxury" }
  },
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
			"serverInfo":      map[string]any{"name": "agent-plane-example-mcp", "version": "0.1.0"},
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
				{
					"name":        "search_attractions",
					"description": "Search tourist attractions in a city, optionally filtered by traveler preference (history, nature, food).",
					"inputSchema": json.RawMessage(attractionsToolSchema),
				},
				{
					"name":        "search_hotels",
					"description": "Search hotels in a city by accommodation type (budget, comfort, luxury).",
					"inputSchema": json.RawMessage(hotelsToolSchema),
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
	case "search_attractions":
		handleSearchAttractions(w, req, params.Arguments)
	case "search_hotels":
		handleSearchHotels(w, req, params.Arguments)
	default:
		writeErr(w, req.ID, -32602, "unknown tool: "+params.Name)
	}
}

// unknownArg is the placeholder echoed back when a required string argument
// is missing from a tools/call.
const unknownArg = "unknown"

func handleGetWeather(w http.ResponseWriter, req *rpcRequest, args map[string]any) {
	city, _ := args["city"].(string)
	if city == "" {
		city = unknownArg
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

// handleSearchAttractions returns a canned attraction list per preference —
// deterministic stand-in for a real POI search (e.g. Amap text search).
func handleSearchAttractions(w http.ResponseWriter, req *rpcRequest, args map[string]any) {
	city, _ := args["city"].(string)
	if city == "" {
		city = unknownArg
	}
	pref, _ := args["preference"].(string)
	byPref := map[string][]map[string]any{
		"history": {
			{"name": "Old Palace Museum", "rating": 4.8, "hours": "08:30-17:00", "ticket": "60 CNY", "visitTime": "3h"},
			{"name": "Ancient City Wall", "rating": 4.6, "hours": "08:00-18:00", "ticket": "45 CNY", "visitTime": "2h"},
			{"name": "Heritage Temple", "rating": 4.5, "hours": "07:30-17:30", "ticket": "30 CNY", "visitTime": "1.5h"},
		},
		"nature": {
			{"name": "Lakeside National Park", "rating": 4.7, "hours": "06:00-19:00", "ticket": "free", "visitTime": "4h"},
			{"name": "Fragrant Hills", "rating": 4.5, "hours": "07:00-18:00", "ticket": "15 CNY", "visitTime": "3h"},
			{"name": "Botanical Garden", "rating": 4.4, "hours": "08:00-17:00", "ticket": "10 CNY", "visitTime": "2h"},
		},
		"food": {
			{"name": "Night Food Street", "rating": 4.6, "hours": "16:00-24:00", "ticket": "free", "visitTime": "2h"},
			{"name": "Time-honored Restaurant Row", "rating": 4.4, "hours": "10:00-22:00", "ticket": "free", "visitTime": "2h"},
			{"name": "Local Snack Market", "rating": 4.3, "hours": "09:00-21:00", "ticket": "free", "visitTime": "1.5h"},
		},
	}
	list, ok := byPref[pref]
	if !ok { // default mix
		list = append(append([]map[string]any{}, byPref["history"][0]), byPref["nature"][0], byPref["food"][0])
	}
	payload, _ := json.Marshal(map[string]any{"city": city, "preference": pref, "attractions": list})
	writeResult(w, req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(payload)}},
		"isError": false,
	})
}

// handleSearchHotels returns canned hotels per accommodation type.
func handleSearchHotels(w http.ResponseWriter, req *rpcRequest, args map[string]any) {
	city, _ := args["city"].(string)
	if city == "" {
		city = unknownArg
	}
	typ, _ := args["type"].(string)
	byType := map[string][]map[string]any{
		"budget": {
			{"name": "City Youth Hostel", "rating": 4.2, "price": "180 CNY/night", "location": "near metro line 2"},
			{"name": "Express Inn Downtown", "rating": 4.0, "price": "260 CNY/night", "location": "city center"},
		},
		"comfort": {
			{"name": "Garden Boutique Hotel", "rating": 4.5, "price": "520 CNY/night", "location": "old town"},
			{"name": "Riverside Business Hotel", "rating": 4.4, "price": "480 CNY/night", "location": "river district"},
		},
		"luxury": {
			{"name": "Grand Lakeview Hotel", "rating": 4.8, "price": "1480 CNY/night", "location": "lakefront"},
			{"name": "Imperial Palace Hotel", "rating": 4.7, "price": "1280 CNY/night", "location": "CBD"},
		},
	}
	list, ok := byType[typ]
	if !ok {
		list = byType["comfort"]
	}
	payload, _ := json.Marshal(map[string]any{"city": city, "type": typ, "hotels": list})
	writeResult(w, req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(payload)}},
		"isError": false,
	})
}

func handleGetOrderStatus(w http.ResponseWriter, req *rpcRequest, args map[string]any) {
	orderID, _ := args["orderId"].(string)
	if orderID == "" {
		orderID = unknownArg
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
