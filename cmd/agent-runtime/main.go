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

// webPage is the serve-mode chat UI. Vanilla single-file on purpose — the
// runtime image stays scratch-ish and the page has no build step. Two things
// beyond plain chat: a small markdown renderer (code blocks, lists, emphasis)
// and fenced ```html blocks rendered as artifact cards with preview/download,
// which is how a Skill-produced single-file page (e.g. travel-plan-viz)
// reaches the user through a chat answer.
const webPage = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Agent Plane</title>
<style>
  :root{
    color-scheme:light dark;
    --bg:#f7f7f8; --panel:#ffffff; --ink:#1f2328; --muted:#6e7781; --line:#e4e7eb;
    --accent:#4f46e5; --accent-ink:#ffffff; --user:#eef0f6; --code:#f6f8fa;
    --radius:14px;
  }
  @media (prefers-color-scheme:dark){
    :root{ --bg:#151617; --panel:#1d1e20; --ink:#e8eaed; --muted:#9aa0a6; --line:#2c2e31;
           --accent:#7c74ff; --user:#26272a; --code:#222325; }
  }
  *{box-sizing:border-box}
  body{margin:0;font-family:ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,"PingFang SC","Microsoft YaHei",sans-serif;
       background:var(--bg);color:var(--ink);font-size:15px;line-height:1.6}
  header{position:sticky;top:0;z-index:5;background:color-mix(in srgb,var(--panel) 88%,transparent);
         backdrop-filter:blur(8px);border-bottom:1px solid var(--line);padding:12px 20px;display:flex;align-items:center;gap:12px}
  .logo{width:30px;height:30px;border-radius:9px;background:linear-gradient(135deg,var(--accent),#22d3ee);
        display:flex;align-items:center;justify-content:center;color:#fff;font-weight:700;font-size:13px;flex:none}
  header .t{font-weight:600}
  header .sub{font-size:12px;color:var(--muted)}
  #chips{margin-left:auto;display:flex;flex-wrap:wrap;gap:6px;justify-content:flex-end}
  .chip{font-size:11.5px;color:var(--muted);border:1px solid var(--line);border-radius:999px;padding:2px 9px;background:var(--panel)}
  #log{max-width:780px;margin:0 auto;padding:28px 18px 130px;display:flex;flex-direction:column;gap:22px}
  .row{display:flex;gap:12px}
  .row.user{justify-content:flex-end}
  .av{width:30px;height:30px;border-radius:9px;flex:none;display:flex;align-items:center;justify-content:center;font-size:14px;margin-top:2px}
  .row.agent .av{background:linear-gradient(135deg,var(--accent),#22d3ee);color:#fff}
  .row.user .av{background:var(--user);border:1px solid var(--line);color:var(--muted);font-size:12px}
  .body{min-width:0;max-width:86%}
  .row.user .body{background:var(--user);border:1px solid var(--line);border-radius:var(--radius);padding:10px 14px;white-space:pre-wrap}
  .name{font-size:12px;color:var(--muted);margin-bottom:4px}
  .msg p{margin:0 0 .55em}
  .msg p:last-child{margin-bottom:0}
  .msg h1,.msg h2,.msg h3{margin:.7em 0 .4em;font-size:1.06em}
  .msg ul,.msg ol{margin:.2em 0 .6em;padding-left:1.4em}
  .msg li{margin:.15em 0}
  .msg code{background:var(--code);border:1px solid var(--line);border-radius:5px;padding:.1em .35em;font-size:.88em;
            font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
  .msg pre{background:var(--code);border:1px solid var(--line);border-radius:10px;padding:12px 14px;overflow:auto;margin:.4em 0 .7em}
  .msg pre code{border:0;padding:0;background:none;font-size:.85em}
  .msg a{color:var(--accent)}
  .art{border:1px solid var(--line);border-radius:12px;overflow:hidden;margin:.5em 0 .8em;background:var(--panel)}
  .art-h{display:flex;align-items:center;gap:8px;padding:9px 12px;border-bottom:1px solid var(--line);font-size:13px}
  .art-h .ic{color:var(--accent)}
  .art-h .fn{font-weight:600}
  .art-h .meta{color:var(--muted);font-size:11.5px}
  .art-h .sp{flex:1}
  .art button{border:1px solid var(--line);background:var(--panel);color:var(--ink);border-radius:8px;padding:4px 11px;
              font-size:12.5px;cursor:pointer}
  .art button:hover{border-color:var(--accent);color:var(--accent)}
  .dots{display:inline-flex;gap:5px;padding:6px 0}
  .dots i{width:6px;height:6px;border-radius:50%;background:var(--muted);animation:b 1.2s infinite}
  .dots i:nth-child(2){animation-delay:.2s}.dots i:nth-child(3){animation-delay:.4s}
  @keyframes b{0%,60%,100%{opacity:.25;transform:translateY(0)}30%{opacity:1;transform:translateY(-3px)}}
  footer{position:fixed;bottom:0;left:0;right:0;padding:14px 18px 20px;
         background:linear-gradient(transparent,var(--bg) 30%)}
  .composer{max-width:780px;margin:0 auto;background:var(--panel);border:1px solid var(--line);
            border-radius:18px;padding:10px 12px;display:flex;gap:10px;align-items:flex-end;
            box-shadow:0 4px 18px rgba(0,0,0,.06)}
  textarea{flex:1;border:0;outline:0;resize:none;background:none;color:var(--ink);font:inherit;
           max-height:160px;padding:6px 2px}
  #send{border:0;background:var(--accent);color:var(--accent-ink);border-radius:12px;width:36px;height:36px;
        cursor:pointer;font-size:15px;flex:none;display:flex;align-items:center;justify-content:center}
  #send:disabled{opacity:.4;cursor:default}
  .hint{max-width:780px;margin:6px auto 0;text-align:center;font-size:11.5px;color:var(--muted)}
  #modal{position:fixed;inset:0;background:rgba(0,0,0,.55);display:none;z-index:20;padding:24px}
  #modal.on{display:flex}
  #modal .frame{flex:1;background:var(--panel);border-radius:14px;overflow:hidden;display:flex;flex-direction:column;max-width:1100px;margin:0 auto}
  #modal .bar{display:flex;gap:10px;align-items:center;padding:10px 14px;border-bottom:1px solid var(--line)}
  #modal .bar .t{font-weight:600;flex:1}
  #modal button{border:1px solid var(--line);background:var(--panel);color:var(--ink);border-radius:8px;padding:4px 12px;cursor:pointer}
  #modal iframe{flex:1;border:0;width:100%;background:#fff}
  .err{color:#d05050}
</style></head>
<body>
<header>
  <div class="logo">AP</div>
  <div><div class="t">旅行规划助手</div><div class="sub" id="meta">connecting…</div></div>
  <div id="chips"></div>
</header>
<div id="log"></div>
<footer>
  <div class="composer">
    <textarea id="in" rows="1" placeholder="帮我做一个 4 天 3 晚的北京旅行计划…" autofocus></textarea>
    <button id="send" title="发送">➤</button>
  </div>
  <div class="hint">Enter 发送 · Shift+Enter 换行</div>
</footer>
<div id="modal"><div class="frame">
  <div class="bar"><div class="t">行程页预览</div><button id="dl2">下载 HTML</button><button id="close">关闭</button></div>
  <iframe id="pv"></iframe>
</div></div>
<script>
var sid = localStorage.sid || (localStorage.sid = Math.random().toString(36).slice(2));
var log = document.getElementById('log');

function esc(s){ var d=document.createElement('div'); d.textContent=s; return d.innerHTML; }

// BT is the backtick char, built via escape so this file (a Go raw string)
// never contains a literal one.
var BT='\x60';

// inline markdown over an already-escaped string: code spans, bold, links.
function inline(s){
  var codes=[];
  s=s.split(BT).reduce(function(acc,part,i){ 
    if(i%2===1){ codes.push(part); return acc+'\u0000'+(codes.length-1)+'\u0000'; }
    return acc+part;
  },'');
  s=s.replace(/\*\*([^*]+)\*\*/g,'<strong>$1</strong>');
  s=s.replace(/\[([^\]]+)\]\((https?:[^)\s]+)\)/g,'<a href="$2" target="_blank" rel="noopener">$1</a>');
  s=s.replace(/\u0000(\d+)\u0000/g,function(_,i){ return '<code>'+codes[+i]+'</code>'; });
  return s;
}

// Minimal fenced-block-aware markdown. The html fence is intercepted before
// generic rendering because it becomes an artifact card, not a <pre>.
function md(src){
  var lines=src.replace(/\r/g,'').split('\n');
  var fence=BT+BT+BT, out='', para=[], i=0;
  function flush(){ if(para.length){ out+='<p>'+inline(para.join('<br>'))+'</p>'; para=[]; } }
  while(i<lines.length){
    var l=lines[i];
    if(l.indexOf(fence)===0){
      flush();
      var lang=l.slice(3).trim(), buf=[];
      i++;
      while(i<lines.length && lines[i].indexOf(fence)!==0){ buf.push(lines[i]); i++; }
      i++;
      if(lang==='html'){ out+=artifact(buf.join('\n')); }
      else { out+='<pre><code>'+esc(buf.join('\n'))+'</code></pre>'; }
      continue;
    }
    var h=l.match(/^(#{1,3})\s+(.*)/);
    if(h){ flush(); out+='<h'+h[1].length+'>'+inline(esc(h[2]))+'</h'+h[1].length+'>'; i++; continue; }
    if(/^(\-|\*)\s+/.test(l)||/^\d+\.\s+/.test(l)){
      flush();
      var ord=/^\d+\.\s+/.test(l), items=[];
      while(i<lines.length){
        var m=lines[i].match(ord?/^\d+\.\s+(.*)/:/^(\-|\*)\s+(.*)/);
        if(!m) break;
        items.push('<li>'+inline(esc(m[1]))+'</li>'); i++;
      }
      out+='<'+(ord?'ol':'ul')+'>'+items.join('')+'</'+(ord?'ol':'ul')+'>';
      continue;
    }
    if(l.trim()===''){ flush(); i++; continue; }
    para.push(esc(l)); i++;
  }
  flush();
  return out || '<p>'+esc(src)+'</p>';
}

// An html fence (three backticks + html) becomes a downloadable/previewable
// artifact card — the deliverable of skills like travel-plan-viz is exactly
// such a page.
function artifact(html){
  var id='a'+Math.random().toString(36).slice(2,8);
  var kb=(html.length/1024).toFixed(1);
  return '<div class="art" id="'+id+'"><div class="art-h"><span class="ic">🗺️</span>'+
    '<span class="fn">行程计划页</span><span class="meta">单文件 HTML · '+kb+' KB</span><span class="sp"></span>'+
    '<button data-a="pv">预览</button><button data-a="dl">下载</button></div></div>'+
    '<textarea class="artsrc" style="display:none">'+esc(html)+'</textarea>';
}
document.addEventListener('click', function(e){
  var b=e.target.closest('.art button'); if(!b) return;
  var card=b.closest('.art'), src=card.nextElementSibling.value, name='旅行计划.html';
  if(b.dataset.a==='dl'){
    var u=URL.createObjectURL(new Blob([src],{type:'text/html'}));
    var a=document.createElement('a'); a.href=u; a.download=name; a.click(); URL.revokeObjectURL(u);
  }else{
    document.getElementById('pv').srcdoc=src;
    document.getElementById('modal').classList.add('on');
    document.getElementById('dl2').onclick=function(){
      var u=URL.createObjectURL(new Blob([src],{type:'text/html'}));
      var a=document.createElement('a'); a.href=u; a.download=name; a.click(); URL.revokeObjectURL(u);
    };
  }
});
document.getElementById('close').onclick=function(){
  document.getElementById('modal').classList.remove('on');
  document.getElementById('pv').srcdoc='';
};

function add(kind, text){
  var row=document.createElement('div');
  if(kind==='you'){ row.className='row user'; row.innerHTML='<div class="body">'+esc(text)+'</div><div class="av">你</div>'; }
  else if(kind==='agent'){ row.className='row agent'; row.innerHTML='<div class="av">✈</div><div class="body"><div class="name">Agent</div><div class="msg">'+md(text)+'</div></div>'; }
  else { row.className='row agent'; row.innerHTML='<div class="av">✈</div><div class="body"><div class="dots"><i></i><i></i><i></i></div></div>'; }
  log.appendChild(row); window.scrollTo(0,document.body.scrollHeight); return row;
}

fetch('/api/info').then(r=>r.json()).then(function(i){
  document.getElementById('meta').textContent='model: '+(i.model||'—');
  var c=document.getElementById('chips');
  (i.tools||[]).forEach(function(t){ var s=document.createElement('span'); s.className='chip'; s.textContent=t; c.appendChild(s); });
});

var ta=document.getElementById('in'), btn=document.getElementById('send');
ta.addEventListener('input', function(){ ta.style.height='auto'; ta.style.height=Math.min(ta.scrollHeight,160)+'px'; });
ta.addEventListener('keydown', function(e){ if(e.key==='Enter'&&!e.shiftKey){ e.preventDefault(); send(); } });
btn.onclick=send;

function send(){
  var msg=ta.value.trim(); if(!msg||btn.disabled) return;
  add('you',msg); ta.value=''; ta.style.height='auto'; btn.disabled=true;
  var th=add('think');
  fetch('/api/chat',{method:'POST',headers:{'content-type':'application/json'},
    body:JSON.stringify({sessionId:sid,message:msg})})
    .then(function(r){return r.json()})
    .then(function(j){ th.remove(); add('agent', j.answer || ('⚠ '+(j.error||'unknown error'))); })
    .catch(function(err){ th.remove(); add('agent','⚠ '+err); });
  btn.disabled=false; ta.focus();
}
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
