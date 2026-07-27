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

// JSON field keys and well-known values reused across MCP responses.
const (
	keyName        = "name"
	keyDescription = "description"
	keyInputSchema = "inputSchema"
	keyContent     = "content"
	keyIsError     = "isError"
	keyHours       = "hours"
	keyLocation    = "location"
	keyType        = "type"
	keyText        = "text"
	keyRating      = "rating"
	keyTicket      = "ticket"
	keyVisitTime   = "visitTime"
	keyPrice       = "price"
	ticketFree     = "free"
)

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
			"serverInfo":      map[string]any{keyName: "agent-plane-example-mcp", "version": "0.1.0"},
		})
	case "tools/list":
		writeResult(w, req.ID, map[string]any{
			"tools": []map[string]any{
				{
					keyName:        "get_order_status",
					keyDescription: "Look up the delivery status of a customer order by its order id.",
					keyInputSchema: json.RawMessage(orderToolSchema),
				},
				{
					keyName:        "get_weather",
					keyDescription: "Get the current weather for a city.",
					keyInputSchema: json.RawMessage(weatherToolSchema),
				},
				{
					keyName:        "search_attractions",
					keyDescription: "Search tourist attractions in a city, optionally filtered by traveler preference (history, nature, food).",
					keyInputSchema: json.RawMessage(attractionsToolSchema),
				},
				{
					keyName:        "search_hotels",
					keyDescription: "Search hotels in a city by accommodation type (budget, comfort, luxury).",
					keyInputSchema: json.RawMessage(hotelsToolSchema),
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
		keyContent: []map[string]any{{keyType: keyText, keyText: text}},
		keyIsError: false,
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
			{keyName: "Old Palace Museum", keyRating: 4.8, keyHours: "08:30-17:00", keyTicket: "60 CNY", keyVisitTime: "3h"},
			{keyName: "Ancient City Wall", keyRating: 4.6, keyHours: "08:00-18:00", keyTicket: "45 CNY", keyVisitTime: "2h"},
			{keyName: "Heritage Temple", keyRating: 4.5, keyHours: "07:30-17:30", keyTicket: "30 CNY", keyVisitTime: "1.5h"},
		},
		"nature": {
			{keyName: "Lakeside National Park", keyRating: 4.7, keyHours: "06:00-19:00", keyTicket: ticketFree, keyVisitTime: "4h"},
			{keyName: "Fragrant Hills", keyRating: 4.5, keyHours: "07:00-18:00", keyTicket: "15 CNY", keyVisitTime: "3h"},
			{keyName: "Botanical Garden", keyRating: 4.4, keyHours: "08:00-17:00", keyTicket: "10 CNY", keyVisitTime: "2h"},
		},
		"food": {
			{keyName: "Night Food Street", keyRating: 4.6, keyHours: "16:00-24:00", keyTicket: ticketFree, keyVisitTime: "2h"},
			{keyName: "Time-honored Restaurant Row", keyRating: 4.4, keyHours: "10:00-22:00", keyTicket: ticketFree, keyVisitTime: "2h"},
			{keyName: "Local Snack Market", keyRating: 4.3, keyHours: "09:00-21:00", keyTicket: ticketFree, keyVisitTime: "1.5h"},
		},
	}
	list, ok := byPref[pref]
	if !ok { // default mix
		list = append(append([]map[string]any{}, byPref["history"][0]), byPref["nature"][0], byPref["food"][0])
	}
	payload, _ := json.Marshal(map[string]any{"city": city, "preference": pref, "attractions": list})
	writeResult(w, req.ID, map[string]any{
		keyContent: []map[string]any{{keyType: keyText, keyText: string(payload)}},
		keyIsError: false,
	})
}

// handleSearchHotels returns canned hotels per accommodation type.
func handleSearchHotels(w http.ResponseWriter, req *rpcRequest, args map[string]any) {
	city, _ := args["city"].(string)
	if city == "" {
		city = unknownArg
	}
	typ, _ := args[keyType].(string)
	byType := map[string][]map[string]any{
		"budget": {
			{keyName: "City Youth Hostel", keyRating: 4.2, keyPrice: "180 CNY/night", keyLocation: "near metro line 2"},
			{keyName: "Express Inn Downtown", keyRating: 4.0, keyPrice: "260 CNY/night", keyLocation: "city center"},
		},
		"comfort": {
			{keyName: "Garden Boutique Hotel", keyRating: 4.5, keyPrice: "520 CNY/night", keyLocation: "old town"},
			{keyName: "Riverside Business Hotel", keyRating: 4.4, keyPrice: "480 CNY/night", keyLocation: "river district"},
		},
		"luxury": {
			{keyName: "Grand Lakeview Hotel", keyRating: 4.8, keyPrice: "1480 CNY/night", keyLocation: "lakefront"},
			{keyName: "Imperial Palace Hotel", keyRating: 4.7, keyPrice: "1280 CNY/night", keyLocation: "CBD"},
		},
	}
	list, ok := byType[typ]
	if !ok {
		list = byType["comfort"]
	}
	payload, _ := json.Marshal(map[string]any{"city": city, keyType: typ, "hotels": list})
	writeResult(w, req.ID, map[string]any{
		keyContent: []map[string]any{{keyType: keyText, keyText: string(payload)}},
		keyIsError: false,
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
		keyContent: []map[string]any{{keyType: keyText, keyText: string(payload)}},
		keyIsError: false,
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
