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
// Agent Plane control plane end-to-end. It stands in for a real Agent framework:
// it pulls resolved config from the Registry (never the API server), reads the
// model key from the referenced Secret via its own RBAC, reads the system
// prompt from the PromptTemplate, then runs a tool-calling loop (see
// internal/agentloop) that actually invokes http and mcp Tools.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"

	"github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
	"github.com/hkmdxlftjf/agent-plane/internal/agentloop"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

type toolView struct {
	Name        string          `json:"name"`
	Type        string          `json:"type"`
	Description string          `json:"description"`
	Endpoint    string          `json:"endpoint"`
	MCPToolName string          `json:"mcpToolName"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type registryConfig struct {
	Namespace  string     `json:"namespace"`
	Name       string     `json:"name"`
	Phase      string     `json:"phase"`
	ConfigHash string     `json:"configHash"`
	Tools      []toolView `json:"tools"`
	Skills     []string   `json:"skills"`
	Model      *struct {
		Provider   string `json:"provider"`
		ModelName  string `json:"modelName"`
		Endpoint   string `json:"endpoint"`
		SecretName string `json:"secretName"`
		SecretKey  string `json:"secretKey"`
	} `json:"model"`
	Spec struct {
		PromptRef *struct {
			Name string `json:"name"`
		} `json:"promptRef"`
	} `json:"spec"`
}

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
	flag.IntVar(&maxSteps, "max-steps", 5, "max tool-calling turns")
	flag.BoolVar(&watch, "watch", os.Getenv("AGENTPLANE_WATCH") == "1", "long-running mode: subscribe to the Registry and hot-reload config")
	flag.BoolVar(&chatMode, "chat", false, "interactive multi-turn chat with the agent (REPL over stdin)")
	flag.BoolVar(&serveMode, "serve", os.Getenv("AGENTPLANE_SERVE") == "1", "web mode: serve a browser chat UI + HTTP API")
	flag.StringVar(&serveAddr, "addr", ":"+envOr("PORT", "8080"), "web mode listen address")
	flag.Var(overrides, "tool-endpoint", "override a tool endpoint: name=url (repeatable)")
	flag.Parse()

	ctx := context.Background()

	// Long-running data-plane mode: subscribe to the Registry and hot-reload.
	// This is how an operator-materialized runtime pod runs (pull model).
	// Serve mode takes precedence when both are set (the image CMD defaults to
	// --watch, so AGENTPLANE_SERVE=1 switches a deployed pod to web mode).
	if watch && !serveMode {
		watchLoop(ctx, registry, ns, name)
		return
	}

	cfg, err := fetchConfig(ctx, registry, ns, name)
	if err != nil {
		fatal("fetch config from registry", err)
	}
	fmt.Printf("▶ Registry config for %s/%s (phase=%s)\n", cfg.Namespace, cfg.Name, cfg.Phase)
	if cfg.Phase != "Ready" {
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

	kcfg, err := ctrl.GetConfig()
	if err != nil {
		fatal("load kubeconfig", err)
	}
	k, err := client.New(kcfg, client.Options{Scheme: scheme})
	if err != nil {
		fatal("build k8s client", err)
	}

	apiKey, err := readSecretKey(ctx, k, ns, cfg.Model.SecretName, cfg.Model.SecretKey)
	if err != nil {
		fatal("read model credential secret", err)
	}

	system := "You are a helpful assistant. Use tools when they can answer the question."
	if cfg.Spec.PromptRef != nil {
		var pt v1alpha1.PromptTemplate
		if err := k.Get(ctx, client.ObjectKey{Namespace: ns, Name: cfg.Spec.PromptRef.Name}, &pt); err == nil && pt.Spec.System != "" {
			system = pt.Spec.System
		}
	}

	endpoint := cfg.Model.Endpoint
	if endpoint == "" && cfg.Model.Provider == "openrouter" {
		endpoint = "https://openrouter.ai/api/v1"
	}

	tools := make([]agentloop.Tool, 0, len(cfg.Tools))
	for _, t := range cfg.Tools {
		tools = append(tools, agentloop.Tool{
			Name: t.Name, Type: t.Type, Description: t.Description,
			Endpoint: t.Endpoint, MCPToolName: t.MCPToolName, InputSchema: t.InputSchema,
		})
	}

	base := agentloop.Config{
		Endpoint: endpoint, APIKey: apiKey, Model: cfg.Model.ModelName,
		System: system, Tools: tools, MaxSteps: maxSteps,
		Logf: func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) },
	}

	// Web mode: serve a browser chat UI + HTTP API.
	if serveMode {
		serveHTTP(ctx, name, cfg, base, serveAddr)
		return
	}

	// Interactive chat: multi-turn REPL over stdin.
	if chatMode {
		chatREPL(ctx, name, base)
		return
	}

	fmt.Printf("\n▶ Running agent loop (prompt: %q)\n", prompt)
	base.Prompt = prompt
	answer, err := agentloop.Run(ctx, base)
	if err != nil {
		fatal("agent loop", err)
	}
	fmt.Printf("\n✅ Final answer:\n%s\n", answer)
}

func fetchConfig(ctx context.Context, registry, ns, name string) (*registryConfig, error) {
	url := fmt.Sprintf("%s/v1/agents/%s/%s/config", registry, ns, name)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registry %d: %s", resp.StatusCode, body)
	}
	var cfg registryConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func readSecretKey(ctx context.Context, k client.Client, ns, name, key string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("model has no credential secret; set a Credential+Secret")
	}
	var secret corev1.Secret
	if err := k.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &secret); err != nil {
		return "", err
	}
	v, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("key %q not in secret %q", key, name)
	}
	return string(v), nil
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "✖ %s: %v\n", what, err)
	os.Exit(1)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// chatREPL runs an interactive multi-turn conversation with the agent over
// stdin. History is retained across turns; tool calls happen transparently.
func chatREPL(ctx context.Context, name string, base agentloop.Config) {
	session := agentloop.NewSession(base)
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
		answer, err := session.Send(ctx, line)
		if err != nil {
			fmt.Printf("✖ %v\n", err)
		} else {
			fmt.Printf("agent> %s\n", answer)
		}
		fmt.Print("\nyou> ")
	}
	fmt.Println()
}

// serveHTTP runs the web mode: a browser chat UI plus a small JSON API. Each
// browser session (by X-Session id) gets its own multi-turn agentloop.Session.
func serveHTTP(ctx context.Context, name string, cfg *registryConfig, base agentloop.Config, addr string) {
	var mu sync.Mutex
	sessions := map[string]*agentloop.Session{}
	getSession := func(id string) *agentloop.Session {
		mu.Lock()
		defer mu.Unlock()
		s, ok := sessions[id]
		if !ok {
			s = agentloop.NewSession(base)
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
		answer, err := getSession(req.SessionID).Send(r.Context(), req.Message)
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
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

// watchLoop is the long-running data-plane mode: subscribe to the Registry's
// SSE /watch stream and hot-reload on every config change. It never exits (it
// is the pod's main process); on disconnect it reconnects with backoff. Each
// event is a full snapshot keyed by configHash — the runtime applies only when
// the hash actually changes.
func watchLoop(ctx context.Context, registry, ns, name string) {
	// The runtime reads the model key from the referenced Secret via its own
	// RBAC — the Registry only hands out Secret coordinates, never values.
	var k client.Client
	if kcfg, err := ctrl.GetConfig(); err == nil {
		k, _ = client.New(kcfg, client.Options{Scheme: scheme})
	}

	fmt.Printf("▶ agent-runtime watching %s for %s/%s\n", registry, ns, name)
	url := fmt.Sprintf("%s/v1/agents/%s/%s/watch", registry, ns, name)
	lastHash := "<none>"
	for {
		if err := streamOnce(ctx, k, url, ns, &lastHash); err != nil {
			fmt.Printf("  stream ended (%v); reconnecting in 2s\n", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func streamOnce(ctx context.Context, k client.Client, url, ns string, lastHash *string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024) // configs can be large
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue // keepalive comments / blank lines
		}
		var cfg registryConfig
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cfg); err != nil {
			continue
		}
		if cfg.ConfigHash == *lastHash {
			continue // no change (also filters repeats)
		}
		*lastHash = cfg.ConfigHash
		applyConfig(ctx, k, ns, &cfg)
	}
	return sc.Err()
}

// applyConfig is where a real runtime would atomically swap its in-memory
// model client + tool table. Here it logs what it received, proving the pull +
// hot-reload path end to end.
func applyConfig(ctx context.Context, k client.Client, ns string, cfg *registryConfig) {
	keyLen := 0
	model := "<none>"
	if cfg.Model != nil {
		model = cfg.Model.Provider + "/" + cfg.Model.ModelName
		if k != nil && cfg.Model.SecretName != "" {
			if v, err := readSecretKey(ctx, k, ns, cfg.Model.SecretName, cfg.Model.SecretKey); err == nil {
				keyLen = len(v)
			}
		}
	}
	toolNames := make([]string, 0, len(cfg.Tools))
	for _, t := range cfg.Tools {
		toolNames = append(toolNames, fmt.Sprintf("%s(%s)", t.Name, t.Type))
	}
	fmt.Printf("↻ hot-reload: phase=%s hash=%.12s model=%s keyLen=%d tools=%v skills=%v\n",
		cfg.Phase, cfg.ConfigHash, model, keyLen, toolNames, cfg.Skills)
}
