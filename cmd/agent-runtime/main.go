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

// Command agent-runtime is a MINIMAL but REAL agent runtime used to verify the
// Agent Plane control plane end-to-end. It stands in for a real Agent
// framework, and is built entirely on the runtime SDK
// (github.com/hkmdxlftjf/agent-plane-sdk-go): it pulls resolved config from
// the Registry (never the API server), reads the model key and memory DSN from
// the referenced Secrets via its own RBAC, then runs a tool-calling loop that
// actually invokes http and mcp Tools. A custom runtime does exactly this with
// its own framework in place of the SDK's reference agentloop.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	sdk "github.com/hkmdxlftjf/agent-plane-sdk-go"
	"github.com/hkmdxlftjf/agent-plane-sdk-go/agentloop"
	"github.com/hkmdxlftjf/agent-plane-sdk-go/memory"
	"github.com/hkmdxlftjf/agent-plane-sdk-go/policy"
	"github.com/hkmdxlftjf/agent-plane-sdk-go/retriever"
	"github.com/hkmdxlftjf/agent-plane-sdk-go/secrets"
)

type endpointOverrides map[string]string

func (e endpointOverrides) String() string { return "" }
func (e endpointOverrides) Set(v string) error {
	parts := strings.SplitN(v, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("expected name=url")
	}
	e[parts[0]] = parts[1]
	return nil
}

func main() {
	overrides := endpointOverrides{}
	var registry, ns, name, prompt string
	var maxSteps int
	var watch bool
	var chatMode bool
	var serveMode bool
	var serveAddr string
	flag.StringVar(&registry, "registry", envOr("AGENTPLANE_REGISTRY", "http://localhost:9090"), "Registry base URL")
	flag.StringVar(&ns, "namespace", envOr("AGENTPLANE_AGENT_NAMESPACE", "default"), "Agent namespace")
	flag.StringVar(&name, "name", envOr("AGENTPLANE_AGENT_NAME", "demo-agent"), "Agent name")
	flag.StringVar(&prompt, "prompt", "What is the status of order A-42? Use the available tool.", "user prompt")
	flag.IntVar(&maxSteps, "max-steps", 8, "max tool-calling turns")
	flag.BoolVar(&watch, "watch", os.Getenv("AGENTPLANE_WATCH") == "1", "long-running mode: subscribe to the Registry and hot-reload config")
	flag.BoolVar(&chatMode, "chat", false, "interactive multi-turn chat with the agent (REPL over stdin)")
	flag.BoolVar(&serveMode, "serve", os.Getenv("AGENTPLANE_SERVE") == "1", "web mode: serve a browser chat UI + HTTP API")
	flag.StringVar(&serveAddr, "addr", ":"+envOr("PORT", "8080"), "web mode listen address")
	flag.Var(overrides, "tool-endpoint", "override a tool endpoint: name=url (repeatable)")
	flag.Parse()

	ctx := context.Background()
	rc := sdk.NewClient(registry, sdk.WithLogf(func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) }))

	// Long-running data-plane mode: subscribe to the Registry and hot-reload.
	// This is how an operator-materialized runtime pod runs (pull model).
	// Serve mode takes precedence when both are set (the image CMD defaults to
	// --watch, so AGENTPLANE_SERVE=1 switches a deployed pod to web mode).
	if watch && !serveMode {
		watchLoop(ctx, rc, ns, name)
		return
	}

	cfg, err := rc.FetchConfig(ctx, ns, name)
	if err != nil {
		fatal("fetch config from registry", err)
	}
	fmt.Printf("▶ Registry config for %s/%s (phase=%s)\n", cfg.Namespace, cfg.Name, cfg.Phase)
	if cfg.Phase != sdk.PhaseReady {
		fatal("agent not ready", fmt.Errorf("phase=%s", cfg.Phase))
	}
	if cfg.Model == nil {
		fatal("resolve model", fmt.Errorf("registry returned no model view"))
	}
	fmt.Printf("  model=%s/%s\n", cfg.Model.Provider, cfg.Model.ModelName)

	for i := range cfg.Tools {
		if u, ok := overrides[cfg.Tools[i].Name]; ok {
			cfg.Tools[i].Endpoint = u
		}
		fmt.Printf("  tool: %s (type=%s) -> %s\n", cfg.Tools[i].Name, cfg.Tools[i].Type, cfg.Tools[i].Endpoint)
	}

	sec, err := secrets.NewReader(ns)
	if err != nil {
		fatal("build secret reader", err)
	}
	apiKey, err := sec.Read(ctx, cfg.Model.SecretName, cfg.Model.SecretKey)
	if err != nil {
		fatal("read model credential secret", err)
	}

	system := buildSystemPrompt(cfg, func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) })

	// Open a persistent memory backend if the Agent references one. The runtime
	// reads the connection DSN from the Secret itself (Registry serves only
	// coordinates), mirroring how the model key is read.
	store := openMemory(ctx, sec, cfg.Memories)

	// Build a RAG retriever from any referenced KnowledgeBases. The SDK
	// retriever serves http-source KBs; other sources are declared but their
	// retrieval is left to a real runtime.
	rag := newRetriever(cfg.Knowledge)

	endpoint := cfg.Model.Endpoint
	if endpoint == "" && cfg.Model.Provider == "openrouter" {
		endpoint = "https://openrouter.ai/api/v1"
	}

	base := agentloop.Config{
		Endpoint: endpoint, APIKey: apiKey, Model: cfg.Model.ModelName,
		System: system, Tools: cfg.Tools, MaxSteps: maxSteps,
		Logf: func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) },
	}

	// Build the policy enforcer from the Registry-served view. The Operator has
	// already refused to make this Agent Ready if its *declared* refs are denied;
	// what is left is the call-time half, which only the runtime can do.
	enf := policy.New(cfg.Policy)
	if lines := enf.Describe(); len(lines) > 0 {
		for _, line := range lines {
			fmt.Printf("  policy: %s\n", line)
		}
	} else {
		fmt.Println("  policy: none (no Policy/ToolPolicy referenced)")
	}

	// Web mode: serve a browser chat UI + HTTP API.
	if serveMode {
		serveHTTP(ctx, name, cfg, base, serveAddr, store, rag, enf)
		return
	}

	// Interactive chat: multi-turn REPL over stdin.
	if chatMode {
		chatREPL(ctx, name, cfg, base, store, rag, enf)
		return
	}

	fmt.Printf("\n▶ Running agent loop (prompt: %q)\n", prompt)
	answer, err := agentloop.Run(ctx, newSessionConfig(base, cfg, enf), rag.Augment(ctx, prompt))
	if err != nil {
		fatal("agent loop", err)
	}
	fmt.Printf("\n✅ Final answer:\n%s\n", answer)
}

// newSessionConfig returns a copy of base wired to a *fresh* enforcement
// session: the tool guard and the load_skill tool both close over it.
//
// Both pieces of state are per-conversation, so this cannot be hoisted into the
// shared base. Call caps (maxCallsPerSession) would otherwise be a single global
// budget, and one chat loading a narrow skill would confine every other chat's
// tool calls.
func newSessionConfig(base agentloop.Config, cfg *sdk.AgentConfig, enf *policy.Enforcer) agentloop.Config {
	sess := enf.Session()
	base.ToolGuard = sess.Guard
	// Register the in-process load_skill tool so the model can pull a skill's full
	// instructions into context on demand (progressive disclosure). Loading a
	// skill that declares allowedTools also narrows what this session may call.
	if tool, ok := loadSkillTool(cfg, sess); ok {
		base.LocalTools = map[string]agentloop.LocalTool{"load_skill": tool}
	}
	return base
}

// buildSystemPrompt composes the Registry-resolved PromptTemplate system text
// with a *catalog* of the Agent's Skills. Only each Skill's name + description
// is inlined; the full instruction body is pulled into context on demand via
// the load_skill tool (see loadSkillTool). This keeps the system prompt flat
// regardless of how many skills an Agent mounts.
func buildSystemPrompt(cfg *sdk.AgentConfig, logf func(string, ...any)) string {
	system := "You are a helpful assistant. Use tools when they can answer the question."
	if cfg.Prompt != nil && cfg.Prompt.System != "" {
		system = cfg.Prompt.System
	}
	catalog := make([]string, 0, len(cfg.Skills))
	for _, sk := range cfg.Skills {
		if sk.Content == "" {
			continue // nothing to load
		}
		desc := sk.Description
		if desc == "" {
			desc = sk.Name
		}
		catalog = append(catalog, fmt.Sprintf("- %s: %s", sk.Name, desc))
		logf("skill (catalog): %s (~%d tokens, lazy)", sk.Name, estimateTokens(sk.Content))
	}
	if len(catalog) > 0 {
		system += "\n\n# Skills available\n" +
			"The following skills are available but their full instructions are NOT loaded. " +
			"When a skill is relevant to the user's request, call load_skill(name) to load its " +
			"instructions before acting on that task.\n" +
			strings.Join(catalog, "\n")
	}
	return system
}

// estimateTokens is a dependency-free approximation of token count (roughly 4
// ASCII characters per token, 1 token per non-ASCII rune for CJK and other wide
// scripts) used to size the skill catalog against the model's actual context
// budget rather than raw character counts. It is not a substitute for the
// provider's real tokenizer, which the SDK does not expose.
func estimateTokens(s string) int {
	ascii, wide := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			wide++
		}
	}
	return ascii/4 + wide
}

// loadSkillTool builds the in-process load_skill tool. It serves a named Skill's
// full instructions from the Registry-resolved config on demand, so skill bodies
// enter the model context only when the model asks for them. Loading a skill that
// declares allowedTools also narrows what the session may call from then on,
// which is why the tool is bound to a policy session. The second return is false
// when the Agent has no loadable skill, in which case the tool should not be
// registered.
func loadSkillTool(cfg *sdk.AgentConfig, sess *policy.Session) (agentloop.LocalTool, bool) {
	skills := make(map[string]sdk.Skill, len(cfg.Skills))
	names := make([]string, 0, len(cfg.Skills))
	for _, sk := range cfg.Skills {
		if sk.Content == "" {
			continue
		}
		skills[sk.Name] = sk
		names = append(names, sk.Name)
	}
	if len(skills) == 0 {
		return agentloop.LocalTool{}, false
	}
	return agentloop.LocalTool{
		Description: "Load the full instructions for a named skill from the 'Skills available' " +
			"catalog in the system prompt. Call this before acting on a task the skill covers.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string",` +
			`"description":"skill name from the catalog"}},"required":["name"]}`),
		Handler: func(_ context.Context, argsJSON string) (string, error) {
			var args struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal([]byte(argsJSON), &args)
			sk, ok := skills[args.Name]
			if !ok {
				// Return (not error) so the model can retry with a valid name.
				return fmt.Sprintf("no such skill %q; available skills: %s",
					args.Name, strings.Join(names, ", ")), nil
			}
			// Record the disclosure *before* returning the body: from the model's
			// next tool call onwards, this skill's allowedTools constrain the session.
			if sess != nil {
				sess.NoteSkillLoaded(sk.Name, sk.AllowedTools)
				if tools, scoped := sess.ScopedTools(); scoped {
					fmt.Printf("  skill %q loaded; tools now confined to %v\n", sk.Name, tools)
				}
			}
			return sk.Content, nil
		},
	}, true
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "✖ %s: %v\n", what, err)
	os.Exit(1)
}

// openMemory returns a persistence Store for the first memory the runtime can
// use, or nil if none is configured/usable. The DSN is read from the Secret the
// memory view points at (the Registry never serves the value itself).
func openMemory(ctx context.Context, sec *secrets.Reader, mems []sdk.Memory) memory.Store {
	for _, m := range mems {
		if sec == nil || m.SecretName == "" {
			continue
		}
		dsn, err := sec.Read(ctx, m.SecretName, m.SecretKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  memory %q: read connection secret: %v\n", m.Name, err)
			continue
		}
		store, err := memory.Open(m.Backend, dsn, m.Namespace)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  memory %q: %v\n", m.Name, err)
			continue
		}
		fmt.Printf("  memory: %s (backend=%s namespace=%s)\n", m.Name, m.Backend, m.Namespace)
		return store
	}
	return nil
}

// newRetriever wraps the SDK retriever with the reference runtime's logging.
func newRetriever(kbs []sdk.KnowledgeBase) *retriever.Retriever {
	r := retriever.New(kbs, retriever.WithLogf(func(f string, a ...any) {
		fmt.Fprintf(os.Stderr, "  "+f+"\n", a...)
	}))
	for _, kb := range kbs {
		fmt.Printf("  knowledgeBase: %s (source=%s uri=%s)\n", kb.Name, kb.Source, kb.URI)
	}
	if len(kbs) > 0 && len(r.Unusable()) == len(kbs) {
		fmt.Println("  (no http-source KnowledgeBase; retrieval not performed by the reference runtime)")
	}
	return r
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// chatREPL runs an interactive multi-turn conversation with the agent over
// stdin. History is retained across turns; tool calls happen transparently. If
// a memory store is configured, prior turns are restored on start and every
// exchange is persisted.
func chatREPL(ctx context.Context, name string, cfg *sdk.AgentConfig, base agentloop.Config, store memory.Store, rag *retriever.Retriever, enf *policy.Enforcer) {
	session := agentloop.NewSession(newSessionConfig(base, cfg, enf))
	const sessionID = "cli"
	if store != nil {
		if turns, err := store.Load(ctx, sessionID); err == nil {
			for _, t := range turns {
				session.AppendHistory(t.Role, t.Content)
			}
			if len(turns) > 0 {
				fmt.Printf("(restored %d turns from memory)\n", len(turns))
			}
		}
	}
	fmt.Printf("\n💬 Chatting with agent %q. Type your message; 'exit' or Ctrl-D to quit.\n\n", name)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	fmt.Print("you> ")
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch line {
		case "":
			fmt.Print("you> ")
			continue
		case "exit", "quit":
			fmt.Println("bye")
			return
		}
		answer, err := session.Send(ctx, rag.Augment(ctx, line))
		if err != nil {
			fmt.Printf("✖ %v\n", err)
		} else {
			fmt.Printf("agent> %s\n", answer)
			if store != nil {
				_ = store.Append(ctx, sessionID,
					memory.Turn{Role: "user", Content: line},
					memory.Turn{Role: "assistant", Content: answer})
			}
		}
		fmt.Print("\nyou> ")
	}
	fmt.Println()
}

// serveHTTP runs the web mode: a browser chat UI plus a small JSON API. Each
// browser session (by X-Session id) gets its own multi-turn agentloop.Session,
// and with it its own policy enforcement session, so one chat exhausting a
// tool's maxCallsPerSession budget does not starve another.
// If a memory store is configured, a session's history is restored on first use
// and every exchange is persisted under its session id.
func serveHTTP(ctx context.Context, name string, cfg *sdk.AgentConfig, base agentloop.Config, addr string, store memory.Store, rag *retriever.Retriever, enf *policy.Enforcer) {
	var mu sync.Mutex
	sessions := map[string]*agentloop.Session{}
	getSession := func(reqCtx context.Context, id string) *agentloop.Session {
		mu.Lock()
		defer mu.Unlock()
		s, ok := sessions[id]
		if !ok {
			s = agentloop.NewSession(newSessionConfig(base, cfg, enf))
			if store != nil {
				if turns, err := store.Load(reqCtx, id); err == nil {
					for _, t := range turns {
						s.AppendHistory(t.Role, t.Content)
					}
				}
			}
			sessions[id] = s
		}
		return s
	}

	toolNames := make([]string, 0, len(cfg.Tools))
	for _, t := range cfg.Tools {
		toolNames = append(toolNames, t.Name)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(webPage))
	})
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"agent": name, "model": cfg.Model.ModelName, "tools": toolNames})
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			SessionID string `json:"sessionId"`
			Message   string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Message == "" {
			http.Error(w, "expected {sessionId, message}", http.StatusBadRequest)
			return
		}
		if req.SessionID == "" {
			req.SessionID = "default"
		}
		answer, err := getSession(r.Context(), req.SessionID).Send(r.Context(), rag.Augment(r.Context(), req.Message))
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		if store != nil {
			_ = store.Append(r.Context(), req.SessionID,
				memory.Turn{Role: "user", Content: req.Message},
				memory.Turn{Role: "assistant", Content: answer})
		}
		writeJSON(w, map[string]any{"answer": answer})
	})

	fmt.Printf("▶ agent-runtime web UI for %q on %s (model=%s tools=%v)\n", name, addr, cfg.Model.ModelName, toolNames)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal("web server", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

const webPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Agent Plane Agent</title>
<style>
  :root{color-scheme:light dark}
  body{font-family:system-ui,sans-serif;margin:0;background:#0b1020;color:#e6e9f0}
  header{padding:12px 16px;background:#141b31;border-bottom:1px solid #232c48}
  header b{color:#8ab4ff}
  #meta{font-size:12px;color:#9aa4bf;margin-top:2px}
  #log{max-width:760px;margin:0 auto;padding:16px;display:flex;flex-direction:column;gap:10px}
  .msg{padding:10px 14px;border-radius:12px;max-width:80%;white-space:pre-wrap;line-height:1.45}
  .you{align-self:flex-end;background:#2a5bd7;color:#fff}
  .agent{align-self:flex-start;background:#1c2540;border:1px solid #2c3860}
  .tool{align-self:flex-start;font-size:12px;color:#9aa4bf;font-family:ui-monospace,monospace}
  form{position:sticky;bottom:0;max-width:760px;margin:0 auto;display:flex;gap:8px;padding:12px 16px;background:#0b1020}
  input{flex:1;padding:12px 14px;border-radius:10px;border:1px solid #2c3860;background:#141b31;color:#e6e9f0;font-size:15px}
  button{padding:0 18px;border:0;border-radius:10px;background:#2a5bd7;color:#fff;font-size:15px;cursor:pointer}
  button:disabled{opacity:.5}
</style></head>
<body>
<header><b>Agent Plane Agent</b> — <span id="name">…</span><div id="meta"></div></header>
<div id="log"></div>
<form id="f"><input id="in" placeholder="Ask about an order or the weather…" autocomplete="off" autofocus><button id="send">Send</button></form>
<script>
const sid = localStorage.sid || (localStorage.sid = Math.random().toString(36).slice(2));
const log = document.getElementById('log');
function add(cls, text){ const d=document.createElement('div'); d.className='msg '+cls; d.textContent=text; log.appendChild(d); window.scrollTo(0,document.body.scrollHeight); return d; }
fetch('/api/info').then(r=>r.json()).then(i=>{
  document.getElementById('name').textContent = i.agent;
  document.getElementById('meta').textContent = 'model: '+i.model+'  ·  tools: '+(i.tools||[]).join(', ');
});
const f=document.getElementById('f'), input=document.getElementById('in'), btn=document.getElementById('send');
f.onsubmit=async e=>{
  e.preventDefault();
  const msg=input.value.trim(); if(!msg) return;
  add('you',msg); input.value=''; btn.disabled=true;
  const thinking=add('tool','…thinking');
  try{
    const r=await fetch('/api/chat',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({sessionId:sid,message:msg})});
    const j=await r.json(); thinking.remove();
    add('agent', j.answer || ('error: '+(j.error||'unknown')));
  }catch(err){ thinking.remove(); add('agent','error: '+err); }
  btn.disabled=false; input.focus();
};
</script>
</body></html>`

// watchLoop is the long-running data-plane mode: the SDK client subscribes to
// the Registry's SSE /watch stream (reconnecting with backoff, deduping on
// configHash) and this loop applies every effective change. It never exits —
// it is the pod's main process.
func watchLoop(ctx context.Context, rc *sdk.Client, ns, name string) {
	// The runtime reads the model key from the referenced Secret via its own
	// RBAC — the Registry only hands out Secret coordinates, never values.
	sec, err := secrets.NewReader(ns)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  no kubernetes access (%v); hot-reload will skip Secret reads\n", err)
	}

	fmt.Printf("▶ agent-runtime watching the Registry for %s/%s\n", ns, name)
	_ = rc.Watch(ctx, ns, name, func(cfg *sdk.AgentConfig) {
		applyConfig(ctx, sec, cfg)
	})
}

// applyConfig is where a real runtime would atomically swap its in-memory
// model client + tool table. Here it logs what it received, proving the pull +
// hot-reload path end to end.
func applyConfig(ctx context.Context, sec *secrets.Reader, cfg *sdk.AgentConfig) {
	keyLen := 0
	model := "<none>"
	if cfg.Model != nil {
		model = cfg.Model.Provider + "/" + cfg.Model.ModelName
		if sec != nil && cfg.Model.SecretName != "" {
			if v, err := sec.Read(ctx, cfg.Model.SecretName, cfg.Model.SecretKey); err == nil {
				keyLen = len(v)
			}
		}
	}
	toolNames := make([]string, 0, len(cfg.Tools))
	for _, t := range cfg.Tools {
		toolNames = append(toolNames, fmt.Sprintf("%s(%s)", t.Name, t.Type))
	}
	skillNames := make([]string, 0, len(cfg.Skills))
	for _, sk := range cfg.Skills {
		skillNames = append(skillNames, sk.Name)
	}
	memNames := make([]string, 0, len(cfg.Memories))
	for _, m := range cfg.Memories {
		memNames = append(memNames, fmt.Sprintf("%s(%s)", m.Name, m.Backend))
	}
	// Surface the policy too: a Policy edit changes the Agent's config hash, so
	// it arrives here like any other change, and an operator watching this log
	// should be able to see enforcement tighten or loosen.
	policySummary := "none"
	if lines := policy.New(cfg.Policy).Describe(); len(lines) > 0 {
		policySummary = strings.Join(lines, "; ")
	}
	fmt.Printf("↻ hot-reload: phase=%s hash=%.12s model=%s keyLen=%d tools=%v skills=%v memories=%v policy=[%s]\n",
		cfg.Phase, cfg.ConfigHash, model, keyLen, toolNames, skillNames, memNames, policySummary)
}
