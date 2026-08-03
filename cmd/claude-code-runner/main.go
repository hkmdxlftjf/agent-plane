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

// Command claude-code-runner runs Claude Code as an Agent Plane runtime.
//
// It is a thin shell, not an agent: Claude Code owns the loop, the tools, and
// the context. This process only translates between the two worlds —
//
//	Registry config  ──projection──>  CLAUDE.md + .mcp.json in the working tree
//	POST /api/chat   ──exec────────>  claude --print --output-format json
//
// The projection is the whole idea. Claude Code will not accept an injected
// tool list or system prompt over an API, but it *does* read an instruction file
// and an MCP config from its working directory — so the platform's declarations
// are written into the forms the CLI already understands. Nothing in the CLI is
// patched, and the same approach carries to any other coding agent that reads
// files on startup.
//
// What this deliberately does not do: interpret the Workflow graph (Claude Code
// has its own loop) or use the SDK's agentloop.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	sdk "github.com/hkmdxlftjf/agent-plane-sdk-go"
	"github.com/hkmdxlftjf/agent-plane-sdk-go/policy"
	"github.com/hkmdxlftjf/agent-plane-sdk-go/secrets"
)

func main() {
	var registry, ns, name, workspace, cliPath, addr string
	var timeout time.Duration
	flag.StringVar(&registry, "registry", envOr("AGENTPLANE_REGISTRY", "http://localhost:9090"), "Registry base URL")
	flag.StringVar(&ns, "namespace", envOr("AGENTPLANE_AGENT_NAMESPACE", "default"), "Agent namespace")
	flag.StringVar(&name, "name", envOr("AGENTPLANE_AGENT_NAME", ""), "Agent name")
	flag.StringVar(&workspace, "workspace", envOr("AGENTPLANE_WORKSPACE", "/workspace"), "working tree the agent operates in")
	flag.StringVar(&cliPath, "cli", envOr("AGENTPLANE_CLAUDE_CLI", "claude"), "path to the Claude Code CLI")
	flag.StringVar(&addr, "addr", ":"+envOr("PORT", "8080"), "listen address")
	flag.DurationVar(&timeout, "turn-timeout", 30*time.Minute, "how long one turn may run")
	flag.Parse()

	if name == "" {
		fatal("agent name", fmt.Errorf("set --name or AGENTPLANE_AGENT_NAME"))
	}

	ctx := context.Background()
	rc := sdk.NewClient(registry, sdk.WithLogf(func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) }))

	cfg, err := rc.FetchConfig(ctx, ns, name)
	if err != nil {
		fatal("fetch config from registry", err)
	}
	fmt.Printf("▶ %s/%s (phase=%s)\n", cfg.Namespace, cfg.Name, cfg.Phase)
	if cfg.Phase != sdk.PhaseReady {
		fatal("agent not ready", fmt.Errorf("phase=%s", cfg.Phase))
	}

	sec, err := secrets.NewReader(ns)
	if err != nil {
		// Not fatal: a runner given its model key directly through the
		// environment still works, and saying so beats failing to start.
		fmt.Fprintf(os.Stderr, "  no kubernetes access (%v); reading credentials from the environment only\n", err)
	}

	env, err := modelEnv(ctx, sec, cfg.Model)
	if err != nil {
		fatal("resolve model credential", err)
	}

	r := &runner{
		cfg:       cfg,
		workspace: workspace,
		cli:       cliPath,
		env:       env,
		timeout:   timeout,
		enforcer:  policy.New(cfg.Policy),
		sessions:  map[string]string{},
	}

	if err := r.project(); err != nil {
		fatal("project config into the working tree", err)
	}

	// The policy view is enforced by the control plane for declarations and, for
	// tool calls, by Claude Code's own permission flags — see toolPermissions.
	// Log what is in effect so the two halves are visible in one place.
	for _, line := range r.enforcer.Describe() {
		fmt.Printf("  policy: %s\n", line)
	}

	// Hot reload: the Agent's config hash changes when any dependency does, so
	// re-project rather than restart. Claude Code re-reads CLAUDE.md and .mcp.json
	// on its next invocation, so a change lands on the following turn.
	go func() {
		_ = rc.Watch(ctx, ns, name, func(fresh *sdk.AgentConfig) {
			r.mu.Lock()
			r.cfg = fresh
			r.enforcer = policy.New(fresh.Policy)
			r.mu.Unlock()
			if err := r.project(); err != nil {
				fmt.Fprintf(os.Stderr, "  re-project failed: %v\n", err)
				return
			}
			fmt.Printf("↻ hot-reload: hash=%.12s\n", fresh.ConfigHash)
		})
	}()

	r.serve(ctx, addr)
}

// toolTypeMCP is the only Tool type this runtime can project: Claude Code
// reaches tools over MCP, and an http Tool has no representation it could use.
const toolTypeMCP = "mcp"

// runner holds everything one agent pod needs.
type runner struct {
	mu        sync.RWMutex
	cfg       *sdk.AgentConfig
	enforcer  *policy.Enforcer
	workspace string
	cli       string
	env       []string
	timeout   time.Duration

	// sessions maps an inbound sessionId to the CLI session it resumes. Claude
	// Code keeps conversation state itself; this only remembers which of its
	// sessions belongs to which caller.
	sessions map[string]string
}

// modelEnv builds the environment the CLI authenticates with. The Registry
// serves Secret *coordinates*; the value is read here through the pod's own
// RBAC and handed to the child process, never written to the working tree.
//
// The Model's endpoint becomes ANTHROPIC_BASE_URL, which is what lets an Agent
// run against a gateway or proxy rather than the public API. Dropping it would
// silently send every request to api.anthropic.com no matter what the Model
// declared.
//
// Which variable carries the credential depends on the endpoint. The public API
// authenticates with an API key (ANTHROPIC_API_KEY); gateways commonly expect a
// bearer token instead (ANTHROPIC_AUTH_TOKEN), and sending a bearer token as an
// API key fails with an unhelpful 401. Setting both is worse than either — the
// CLI rejects the request outright — so exactly one is set. See
// credentialEnvName for which, and how to override it.
func modelEnv(ctx context.Context, sec *secrets.Reader, model *sdk.Model) ([]string, error) {
	env := os.Environ()
	if model == nil {
		return env, nil
	}
	if model.Endpoint != "" {
		env = append(env, "ANTHROPIC_BASE_URL="+model.Endpoint)
	}
	if model.ModelName != "" {
		env = append(env, "ANTHROPIC_MODEL="+model.ModelName)
	}
	if model.SecretName == "" || sec == nil {
		return env, nil
	}
	key, err := sec.Read(ctx, model.SecretName, model.SecretKey)
	if err != nil {
		return nil, err
	}
	env = append(env, credentialEnvName(model)+"="+key)
	return env, nil
}

// credentialEnvName picks the variable the credential is delivered in.
//
// A custom endpoint is the signal for the bearer form: the public API is the
// only one this runner can assume takes an x-api-key, and every gateway we have
// seen expects an Authorization bearer token. An Agent pointing at its own
// endpoint that genuinely wants the API-key form can set the env var directly
// through spec.runtime.env, which wins because it is already in os.Environ().
func credentialEnvName(model *sdk.Model) string {
	if model.Endpoint != "" {
		return "ANTHROPIC_AUTH_TOKEN"
	}
	return "ANTHROPIC_API_KEY"
}

// project writes the Registry config into the files Claude Code reads on
// startup. Called once before serving and again on every hot reload.
func (r *runner) project() error {
	r.mu.RLock()
	cfg := r.cfg
	r.mu.RUnlock()

	if err := os.MkdirAll(r.workspace, 0o755); err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	if err := os.WriteFile(filepath.Join(r.workspace, "CLAUDE.md"), []byte(buildInstructions(cfg)), 0o644); err != nil {
		return fmt.Errorf("write CLAUDE.md: %w", err)
	}
	mcp, err := buildMCPConfig(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.workspace, ".mcp.json"), mcp, 0o644); err != nil {
		return fmt.Errorf("write .mcp.json: %w", err)
	}
	return nil
}

// buildInstructions renders the Agent's prompt and Skills as CLAUDE.md.
//
// Skill bodies are inlined here rather than disclosed on demand: Claude Code
// has its own skill mechanism and no load_skill tool to call, so the catalog
// approach the reference runtime uses does not apply. An Agent that mounts many
// large Skills therefore carries them in every turn's context — prefer a few
// focused Skills over many broad ones on this runtime.
func buildInstructions(cfg *sdk.AgentConfig) string {
	var b strings.Builder
	b.WriteString("<!-- Generated by agent-plane claude-code-runner. Edits are overwritten on config change. -->\n\n")

	if cfg.Prompt != nil && cfg.Prompt.System != "" {
		b.WriteString(cfg.Prompt.System)
		b.WriteString("\n\n")
	}

	for _, sk := range cfg.Skills {
		if sk.Content == "" {
			continue
		}
		fmt.Fprintf(&b, "# Skill: %s\n", sk.Name)
		if sk.Description != "" {
			fmt.Fprintf(&b, "\n%s\n", sk.Description)
		}
		b.WriteString("\n")
		b.WriteString(sk.Content)
		b.WriteString("\n\n")
		if len(sk.AllowedTools) > 0 {
			fmt.Fprintf(&b, "While following this skill, restrict yourself to these tools: %s.\n\n",
				strings.Join(sk.AllowedTools, ", "))
		}
	}

	if peers := peerTools(cfg); len(peers) > 0 {
		b.WriteString("# Other repositories\n\n")
		b.WriteString("You own one repository. These MCP servers are other agents, each owning a\n")
		b.WriteString("repository of its own — ask them instead of guessing about code you cannot see:\n\n")
		for _, p := range peers {
			desc := p.Description
			if desc == "" {
				desc = "another repository's agent"
			}
			fmt.Fprintf(&b, "- **%s** — %s\n", p.Name, desc)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// peerTools returns the mcp tools, which is how both MCP servers and peer
// Agents arrive. The Registry has already resolved each to an endpoint, so the
// runner does not need to know which kind it was.
func peerTools(cfg *sdk.AgentConfig) []sdk.Tool {
	var out []sdk.Tool
	for _, t := range cfg.Tools {
		if t.Type == toolTypeMCP && t.Endpoint != "" {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// mcpServerEntry is one entry of Claude Code's .mcp.json.
type mcpServerEntry struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// buildMCPConfig renders the Agent's mcp tools as Claude Code's .mcp.json.
//
// http tools are deliberately omitted: Claude Code has no way to call a bare
// HTTP endpoint as a tool, and emitting an entry it cannot honor would advertise
// a capability that silently fails. Expose such a tool through an MCP server if
// a coding agent needs it.
func buildMCPConfig(cfg *sdk.AgentConfig) ([]byte, error) {
	servers := map[string]mcpServerEntry{}
	for _, t := range peerTools(cfg) {
		servers[t.Name] = mcpServerEntry{Type: "http", URL: t.Endpoint}
	}
	doc := map[string]any{"mcpServers": servers}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal .mcp.json: %w", err)
	}
	return append(out, '\n'), nil
}

// toolPermissions maps the effective policy onto the CLI's permission flags.
//
// This is a partial mapping, and the gap is worth naming: Policy and ToolPolicy
// name Tool CRs, while Claude Code's built-in tools (Bash, Write, …) are not Tool
// CRs at all. A ToolPolicy therefore governs the MCP tools this runner projects
// and says nothing about the built-ins. maxCallsPerSession has no CLI equivalent
// and is not enforced here either.
//
// Returning the unenforceable parts lets the caller report them rather than
// letting a policy look effective when half of it is not.
func toolPermissions(cfg *sdk.AgentConfig, enf *policy.Enforcer) (allowed, disallowed, unenforced []string) {
	if cfg.Policy == nil {
		return nil, nil, nil
	}
	for _, t := range peerTools(cfg) {
		// Claude Code names MCP tools "mcp__<server>".
		arg := "mcp__" + t.Name
		if ok, _ := enf.AllowsTool(t.Name); ok {
			allowed = append(allowed, arg)
		} else {
			disallowed = append(disallowed, arg)
		}
	}
	for _, rule := range cfg.Policy.ToolRules {
		if rule.MaxCallsPerSession != nil {
			unenforced = append(unenforced,
				fmt.Sprintf("maxCallsPerSession on %q (no CLI equivalent)", rule.Tool))
		}
	}
	if cfg.Policy.DefaultToolAction == sdk.ToolActionDeny {
		unenforced = append(unenforced,
			"defaultToolAction=deny does not restrict Claude Code's built-in tools (Bash, Write, …)")
	}
	sort.Strings(allowed)
	sort.Strings(disallowed)
	sort.Strings(unenforced)
	return allowed, disallowed, unenforced
}

// chatRequest is the inbound contract shared with the reference runtime, so a
// Trigger adapter works against either without change.
type chatRequest struct {
	SessionID string `json:"sessionId"`
	Message   string `json:"message"`
}

// cliResult is the subset of `claude --output-format json` this runner reads.
type cliResult struct {
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
}

func (r *runner) serve(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/api/chat", r.handleChat)

	r.mu.RLock()
	allowed, _, unenforced := toolPermissions(r.cfg, r.enforcer)
	r.mu.RUnlock()
	fmt.Printf("▶ claude-code-runner on %s (workspace=%s, mcp tools=%d)\n", addr, r.workspace, len(allowed))
	for _, u := range unenforced {
		fmt.Printf("  ⚠ not enforced by this runtime: %s\n", u)
	}

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal("http server", err)
	}
}

func (r *runner) handleChat(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in chatRequest
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.Message == "" {
		http.Error(w, "expected {sessionId, message}", http.StatusBadRequest)
		return
	}
	if in.SessionID == "" {
		in.SessionID = "default"
	}

	answer, err := r.turn(req.Context(), in.SessionID, in.Message)
	if err != nil {
		// The contract returns errors in the body with HTTP 200 so an adapter can
		// relay them to the user rather than treating them as transport failures.
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"answer": answer})
}

// turn runs one exchange through the CLI.
//
// Turns are serialized: Claude Code edits the single working tree this pod owns,
// so two concurrent invocations would race on the same files. The Operator
// already pins the Deployment to one replica for the same reason; this is the
// in-process half of that guarantee.
func (r *runner) turn(ctx context.Context, sessionID, message string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	args := []string{"--print", "--output-format", "json"}
	if prior, ok := r.sessions[sessionID]; ok {
		// Resuming keeps the CLI's own conversation state, so the caller's
		// sessionId maps to a continuous thread rather than a cold start.
		args = append(args, "--resume", prior)
	}
	allowed, disallowed, _ := toolPermissions(r.cfg, r.enforcer)
	if len(allowed) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowed, ","))
	}
	if len(disallowed) > 0 {
		args = append(args, "--disallowedTools", strings.Join(disallowed, ","))
	}
	args = append(args, message)

	cmd := exec.CommandContext(ctx, r.cli, args...)
	cmd.Dir = r.workspace
	cmd.Env = r.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// The CLI reports its own failures as the JSON envelope on *stdout* and
		// still exits non-zero — "Not logged in", an API error, a bad model. Only
		// reporting stderr therefore surfaces a bare "exit status 1" with the
		// actual reason discarded, which is indistinguishable from the CLI dying
		// silently. Prefer the envelope's message and fall back to stderr.
		return "", fmt.Errorf("claude: %w: %s", err, cliFailureDetail(stdout.Bytes(), stderr.String()))
	}

	var res cliResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return "", fmt.Errorf("parse claude output: %w", err)
	}
	if res.SessionID != "" {
		r.sessions[sessionID] = res.SessionID
	}
	if res.IsError {
		return "", fmt.Errorf("claude reported an error: %s", res.Result)
	}
	return res.Result, nil
}

// cliFailureDetail extracts the reason a failed CLI invocation gives.
//
// The JSON envelope on stdout carries the message ("Not logged in · Please run
// /login", an upstream API error) even when the process exits non-zero, while
// stderr is frequently empty. Unparseable output falls back to stderr, then to
// the raw stdout, so a caller is never handed an empty explanation.
func cliFailureDetail(stdout []byte, stderr string) string {
	var res cliResult
	if err := json.Unmarshal(stdout, &res); err == nil && res.Result != "" {
		return res.Result
	}
	if s := strings.TrimSpace(stderr); s != "" {
		return s
	}
	if s := strings.TrimSpace(string(stdout)); s != "" {
		return s
	}
	return "no output"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "write response: %v\n", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "✖ %s: %v\n", what, err)
	os.Exit(1)
}
