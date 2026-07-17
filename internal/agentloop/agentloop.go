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

// Package agentloop is a minimal, reusable agent execution loop used by the
// verification runtimes (cmd/agent-runtime and cmd/demo). It is deliberately
// NOT part of the CogNet control plane — it stands in for a real Agent
// framework. It calls an OpenAI-compatible chat endpoint and executes tool
// calls against http tools (POST) and mcp tools (JSON-RPC tools/call).
package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Tool is a fully-resolved tool definition the loop can invoke.
type Tool struct {
	Name        string
	Type        string // "http" | "mcp"
	Description string
	Endpoint    string
	MCPToolName string
	InputSchema json.RawMessage
}

// Config drives a single Run of the loop.
type Config struct {
	Endpoint string // OpenAI-compatible base URL (…/v1)
	APIKey   string
	Model    string
	System   string
	Prompt   string
	Tools    []Tool
	MaxSteps int
	// Logf receives human-readable progress lines (optional).
	Logf func(format string, args ...any)
}

func (c *Config) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// Run executes a single-turn tool-calling loop and returns the final answer.
func Run(ctx context.Context, c Config) (string, error) {
	return NewSession(c).Send(ctx, c.Prompt)
}

// Session holds conversation state so a runtime can carry a multi-turn chat.
// Each Send appends the user turn, runs the tool-calling loop (appending
// assistant + tool messages), and returns the model's final answer.
type Session struct {
	endpoint string
	apiKey   string
	model    string
	tools    []Tool
	oaTools  []oaTool
	maxSteps int
	logf     func(format string, args ...any)
	messages []oaMessage
}

// NewSession seeds a session with the system prompt and tool definitions.
func NewSession(c Config) *Session {
	if c.MaxSteps <= 0 {
		c.MaxSteps = 5
	}
	return &Session{
		endpoint: c.Endpoint,
		apiKey:   c.APIKey,
		model:    c.Model,
		tools:    c.Tools,
		oaTools:  buildOpenAITools(c.Tools),
		maxSteps: c.MaxSteps,
		logf:     c.logf,
		messages: []oaMessage{{Role: "system", Content: c.System}},
	}
}

// Send appends a user message, runs the tool-calling loop, and returns the
// model's final answer. Conversation history is retained across calls.
func (s *Session) Send(ctx context.Context, userText string) (string, error) {
	s.messages = append(s.messages, oaMessage{Role: "user", Content: userText})
	for step := 1; step <= s.maxSteps; step++ {
		msg, err := chat(ctx, s.endpoint, s.apiKey, s.model, s.messages, s.oaTools)
		if err != nil {
			return "", err
		}
		s.messages = append(s.messages, *msg)
		if len(msg.ToolCalls) == 0 {
			return msg.Content, nil
		}
		for _, tc := range msg.ToolCalls {
			s.logf("[tool] %s(%s)", tc.Function.Name, tc.Function.Arguments)
			result, err := dispatchTool(ctx, s.tools, tc.Function.Name, tc.Function.Arguments)
			if err != nil {
				result = fmt.Sprintf("tool error: %v", err)
			}
			s.logf("  ↳ %s", truncate(result, 200))
			s.messages = append(s.messages, oaMessage{Role: "tool", ToolCallID: tc.ID, Content: result})
		}
	}
	return "", fmt.Errorf("exceeded max steps (%d) without a final answer", s.maxSteps)
}

// --- OpenAI-compatible chat types -------------------------------------------

type oaMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func buildOpenAITools(tools []Tool) []oaTool {
	out := make([]oaTool, 0, len(tools))
	for _, t := range tools {
		var ot oaTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		if len(t.InputSchema) > 0 {
			ot.Function.Parameters = t.InputSchema
		} else {
			ot.Function.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, ot)
	}
	return out
}

func chat(ctx context.Context, endpoint, apiKey, model string, messages []oaMessage, tools []oaTool) (*oaMessage, error) {
	reqBody := map[string]any{"model": model, "messages": messages, "max_tokens": 512}
	if len(tools) > 0 {
		reqBody["tools"] = tools
		reqBody["tool_choice"] = "auto"
	}
	b, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("HTTP-Referer", "https://github.com/hkmdxlftjf/agent-plane")
	req.Header.Set("X-Title", "CogNet agentloop")

	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Choices []struct {
			Message oaMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("no choices: %s", raw)
	}
	return &out.Choices[0].Message, nil
}

// --- tool dispatch ----------------------------------------------------------

func dispatchTool(ctx context.Context, tools []Tool, name, argsJSON string) (string, error) {
	var tv *Tool
	for i := range tools {
		if tools[i].Name == name {
			tv = &tools[i]
			break
		}
	}
	if tv == nil {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	switch tv.Type {
	case "http":
		return callHTTPTool(ctx, tv.Endpoint, argsJSON)
	case "mcp":
		mcpName := tv.MCPToolName
		if mcpName == "" {
			mcpName = tv.Name
		}
		return callMCPTool(ctx, tv.Endpoint, mcpName, argsJSON)
	default:
		return "", fmt.Errorf("unsupported tool type %q", tv.Type)
	}
}

func callHTTPTool(ctx context.Context, endpoint, argsJSON string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(argsJSON))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return string(body), nil
}

// callMCPTool speaks MCP JSON-RPC over HTTP: initialize, then tools/call.
func callMCPTool(ctx context.Context, endpoint, toolName, argsJSON string) (string, error) {
	if endpoint == "" {
		return "", fmt.Errorf("mcp tool has no resolved endpoint (is the MCPServer Ready?)")
	}
	var args map[string]any
	if argsJSON != "" {
		_ = json.Unmarshal([]byte(argsJSON), &args)
	}
	if _, err := mcpRPC(ctx, endpoint, 1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "cognet-agentloop", "version": "0.1.0"},
	}); err != nil {
		return "", fmt.Errorf("initialize: %w", err)
	}
	res, err := mcpRPC(ctx, endpoint, 2, "tools/call", map[string]any{"name": toolName, "arguments": args})
	if err != nil {
		return "", err
	}
	var parsed struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(res, &parsed); err != nil {
		return string(res), nil
	}
	var sb strings.Builder
	for _, c := range parsed.Content {
		sb.WriteString(c.Text)
	}
	return sb.String(), nil
}

func mcpRPC(ctx context.Context, endpoint string, id int, method string, params any) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("bad json-rpc response: %s", raw)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("mcp error %d: %s", out.Error.Code, out.Error.Message)
	}
	return out.Result, nil
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
