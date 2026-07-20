# Quickstart: Build a Custom Agent

This guide takes you from zero to a running custom agent on Agent Plane. You will:

1. **Declare** the agent and its capabilities as Kubernetes resources (Model, Tool,
   Prompt, Agent).
2. **Implement** an agent runtime — the code that actually runs inference — by
   consuming resolved config from the Registry.
3. **Deploy** that runtime, either yourself or by letting the Operator manage it
   via `spec.runtime`.

> **Mental model.** Agent Plane is a *control plane*. It never does inference. It
> resolves what your agent is made of (a Model + Tools + a Prompt + Skills…) into
> one config document and serves it to your runtime over HTTP. Your runtime is the
> *data plane*: it pulls that config and drives the model/tool loop. Any runtime
> that speaks the [runtime protocol](runtime-protocol.md) plugs in — the reference
> one in `cmd/agent-runtime/` is ~200 lines and is what we build below.

Prerequisites: a cluster with the CRDs + Operator + in-cluster Registry installed.
See [usage.md §3](usage.md) if you haven't done that yet.

---

## Part 1 — Declare the agent (no code)

An Agent references the capabilities it is assembled from. The minimum viable
agent needs a **Model** (required) and a credential **Secret**. Everything else
(Tools, Prompt, Skills, Memory, Policies) is optional and additive.

Save as `my-agent.yaml` and `kubectl apply -f my-agent.yaml -n <namespace>`.

```yaml
# 1. Credential: the LLM API key lives in a plain Secret. Agent Plane never stores
#    key material on its own resources — only coordinates (name/key).
apiVersion: v1
kind: Secret
metadata:
  name: llm-secret
type: Opaque
stringData:
  api-key: "sk-...replace-me..."
---
# 2. Model: which LLM, where, and which Secret holds the key.
apiVersion: core.hkmdxlftjf.io/v1alpha1
kind: Model
metadata:
  name: my-model
spec:
  provider: custom                       # openai | anthropic | openrouter | custom | …
  modelName: claude-haiku-4-5-20251001
  endpoint: https://your-gateway/v1      # OpenAI-compatible base URL
  credentialRef:
    name: my-credential
---
apiVersion: core.hkmdxlftjf.io/v1alpha1
kind: Credential
metadata:
  name: my-credential
spec:
  secretRef:
    name: llm-secret
    key: api-key
---
# 3. Prompt (optional): the system prompt your runtime should use.
apiVersion: core.hkmdxlftjf.io/v1alpha1
kind: PromptTemplate
metadata:
  name: my-prompt
spec:
  system: "You are a helpful assistant. Use tools when they help answer."
---
# 4. Tool (optional): an executable capability the agent can call. This one is a
#    plain HTTP tool; see Part 3 for wiring an MCP server instead.
apiVersion: core.hkmdxlftjf.io/v1alpha1
kind: Tool
metadata:
  name: echo-tool
spec:
  type: http
  description: "Echo back the given text."
  endpoint: http://echo.default.svc:8080/echo
  inputSchema:
    type: object
    properties:
      text: { type: string }
    required: [text]
---
# 5. Agent: assemble the above by reference. modelRef is the only required ref.
apiVersion: core.hkmdxlftjf.io/v1alpha1
kind: Agent
metadata:
  name: my-agent
spec:
  description: My first custom agent.
  modelRef:
    name: my-model
  promptRef:
    name: my-prompt
  toolRefs:
    - name: echo-tool
```

Verify the Operator resolved everything and the config hash is stable:

```sh
kubectl get agent my-agent -n <namespace>
# NAME       PHASE   MODEL      AGE
# my-agent   Ready   my-model   5s
```

`PHASE=Ready` means all references resolved and the config is being served. If it
shows `Degraded`, describe it — a missing referenced resource is the usual cause:

```sh
kubectl describe agent my-agent -n <namespace>   # look at Conditions
```

You can now fetch exactly what any runtime will see:

```sh
kubectl -n agent-plane-system port-forward svc/agent-plane-registry 9090:9090 &
curl -s http://localhost:9090/v1/agents/<namespace>/my-agent/config | jq
```

---

## Part 2 — Implement the runtime (the agent's code)

Your runtime's job is a short, well-defined contract:

1. **Fetch** the resolved config from the Registry — `GET /v1/agents/{ns}/{name}/config`
   (one-shot) or `GET …/watch` (SSE stream for hot-reload). Never talk to the
   Kubernetes API for config.
2. **Read the model key** yourself from the referenced Secret (the config carries
   only `secretName`/`secretKey`, never the value) — using your pod's own RBAC.
3. **Build the model client + tool table** from the config.
4. **Run the loop**: call the model, execute any tool calls it requests, feed
   results back, repeat until the model returns a final answer.
5. *(Optional but recommended)* **Hot-reload**: keep the `/watch` stream open and
   atomically swap the tool table / model client whenever `configHash` changes.

Here is a complete minimal runtime in Go. It has no dependency on Agent Plane
packages — it only speaks HTTP + the OpenAI chat API + the Registry protocol, so
you can port it to any language.

```go
// main.go — a minimal, self-contained Agent Plane runtime.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// AgentConfig mirrors the Registry payload (see docs/runtime-protocol.md §3).
// Ignore fields you don't need; unknown fields are forward-compatible.
type AgentConfig struct {
	Phase      string `json:"phase"`
	ConfigHash string `json:"configHash"`
	Model      *struct {
		ModelName  string `json:"modelName"`
		Endpoint   string `json:"endpoint"`
		SecretName string `json:"secretName"`
		SecretKey  string `json:"secretKey"`
	} `json:"model"`
	Tools []struct {
		Name        string          `json:"name"`
		Type        string          `json:"type"` // "http" | "mcp"
		Description string          `json:"description"`
		Endpoint    string          `json:"endpoint"`
		MCPToolName string          `json:"mcpToolName"`
		InputSchema json.RawMessage `json:"inputSchema"`
	} `json:"tools"`
	Prompt *struct {
		System string `json:"system"`
	} `json:"prompt"`
}

func main() {
	registry := env("AGENTPLANE_REGISTRY", "http://localhost:9090")
	ns := env("AGENTPLANE_AGENT_NAMESPACE", "default")
	name := env("AGENTPLANE_AGENT_NAME", "my-agent")
	ctx := context.Background()

	// 1. Fetch resolved config from the Registry (never the K8s API).
	cfg := mustFetch(ctx, registry, ns, name)
	if cfg.Phase != "Ready" || cfg.Model == nil {
		panic("agent not ready: " + cfg.Phase)
	}

	// 2. Read the model API key from the referenced Secret via your own RBAC.
	//    In-cluster: read /var/run/secrets/... or use the K8s client. Locally you
	//    can inject it directly for testing:
	apiKey := os.Getenv("MODEL_API_KEY") // or: readSecret(ns, cfg.Model.SecretName, cfg.Model.SecretKey)

	// 3. Build tools + system prompt from the config.
	system := "You are a helpful assistant."
	if cfg.Prompt != nil && cfg.Prompt.System != "" {
		system = cfg.Prompt.System // PromptTemplate resolved server-side by the Registry
	}

	// 4. Run one turn of the model+tool loop.
	answer := runLoop(ctx, cfg, system, apiKey, "Say hello and echo the word 'ping'.")
	fmt.Println(answer)
}

// runLoop calls the OpenAI-compatible chat endpoint, executes tool calls the
// model requests, and returns the final answer.
func runLoop(ctx context.Context, cfg *AgentConfig, system, apiKey, prompt string) string {
	msgs := []map[string]any{
		{"role": "system", "content": system},
		{"role": "user", "content": prompt},
	}
	tools := openAITools(cfg) // translate cfg.Tools -> OpenAI function specs

	for step := 0; step < 5; step++ {
		msg := chat(ctx, cfg.Model.Endpoint, apiKey, cfg.Model.ModelName, msgs, tools)
		msgs = append(msgs, msg)
		calls, _ := msg["tool_calls"].([]any)
		if len(calls) == 0 {
			content, _ := msg["content"].(string)
			return content // final answer
		}
		for _, c := range calls {
			call := c.(map[string]any)
			fn := call["function"].(map[string]any)
			result := dispatch(ctx, cfg, fn["name"].(string), fn["arguments"].(string))
			msgs = append(msgs, map[string]any{
				"role": "tool", "tool_call_id": call["id"], "content": result,
			})
		}
	}
	return "(no answer: exceeded max steps)"
}

// dispatch invokes a tool by name. http tools are a plain POST of the JSON args;
// mcp tools speak JSON-RPC (initialize + tools/call) — see the SDK's agentloop.
func dispatch(ctx context.Context, cfg *AgentConfig, name, args string) string {
	for _, t := range cfg.Tools {
		if t.Name != name {
			continue
		}
		switch t.Type {
		case "http":
			return post(ctx, t.Endpoint, args)
		case "mcp":
			return callMCP(ctx, t.Endpoint, orDefault(t.MCPToolName, t.Name), args)
		}
	}
	return "unknown tool: " + name
}

// ---- small HTTP helpers (chat, openAITools, callMCP, post, mustFetch, env) ----
// Elided for brevity; full working versions live in the Go SDK
// (github.com/hkmdxlftjf/agent-plane-sdk-go, package agentloop) and
// cmd/agent-runtime/. The shapes are:
//   chat(...)       POST {endpoint}/chat/completions  -> choices[0].message
//   post(...)       POST {toolEndpoint} with the raw JSON args -> body
//   callMCP(...)    JSON-RPC initialize then tools/call -> concatenated text
//   openAITools(..) map cfg.Tools -> [{type:"function", function:{name,description,parameters}}]

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustFetch(ctx context.Context, registry, ns, name string) *AgentConfig {
	url := fmt.Sprintf("%s/v1/agents/%s/%s/config", registry, ns, name)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		panic(fmt.Sprintf("registry %d: %s", resp.StatusCode, b))
	}
	var cfg AgentConfig
	_ = json.NewDecoder(resp.Body).Decode(&cfg)
	return &cfg
}

func chat(ctx context.Context, endpoint, key, model string, msgs, tools []any) map[string]any {
	body := map[string]any{"model": model, "messages": msgs, "max_tokens": 512}
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	var out struct {
		Choices []struct{ Message map[string]any } `json:"choices"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Choices[0].Message
}
```

> **Don't hand-roll it in Go?** Use the SDK:
> `go get github.com/hkmdxlftjf/agent-plane-sdk-go`. It ships the wire types,
> a `FetchConfig`/`Watch` Registry client (SSE reconnect + `configHash` dedup
> built in), a `secrets.Reader` for RBAC Secret reads, and reference `agentloop`
> (the loop, http + mcp dispatch, multi-turn `Session`), `memory`, and
> `retriever` packages — steps 1–4 become ~30 lines (see the SDK README).
> `cmd/agent-runtime/main.go` in this repo is a complete runtime built on it,
> including `--chat`, `--serve` web UI, and `--watch` hot-reload modes.

### Add hot-reload (optional)

For a long-running pod, subscribe to `/watch` instead of `/config`. Each SSE frame
is a **full snapshot**; compare `configHash` and only rebuild when it changes.
Pseudo-loop (full version: `watchLoop`/`streamOnce` in `cmd/agent-runtime/main.go`):

```text
open SSE GET /v1/agents/{ns}/{name}/watch
for each `data:` frame -> parse AgentConfig:
    if cfg.configHash == lastHash: continue        # includes keepalive comments
    lastHash = cfg.configHash
    tools := build(cfg.tools); key := readSecret(cfg.model.*)
    atomically swap { current, tools, key }        # in-flight requests unaffected
on disconnect: backoff 2s, reconnect               # reconnect => fresh snapshot
```

Because every event is the complete state and last-writer-wins, a dropped event
still converges — your runtime just needs to be idempotent. See
[runtime-protocol.md §5–6](runtime-protocol.md) for the full delivery guarantees.

---

## Part 3 — Deploy the runtime

### Option A: let the Operator manage it (recommended)

Package your runtime as an image whose entrypoint pulls config and runs (the
reference image's default CMD is `--watch`). Then add `spec.runtime` to the Agent
and the Operator materializes a Deployment + Service for you, injecting
`AGENTPLANE_REGISTRY`, `AGENTPLANE_AGENT_NAMESPACE`, and `AGENTPLANE_AGENT_NAME`:

```yaml
apiVersion: core.hkmdxlftjf.io/v1alpha1
kind: Agent
metadata:
  name: my-agent
spec:
  modelRef:   { name: my-model }
  promptRef:  { name: my-prompt }
  toolRefs:   [ { name: echo-tool } ]
  runtime:
    image: my-agent-runtime:dev      # your image; imagePullPolicy IfNotPresent for :dev tags
    replicas: 1
    port: 8080                        # exposes a Service if set
    env:
      - { name: AGENTPLANE_SERVE, value: "1" }   # reference image: switch to web UI mode
```

```sh
kubectl apply -f my-agent.yaml -n <namespace>
kubectl get agent my-agent -n <namespace> -o jsonpath='{.status.runtimeAvailableReplicas}'
```

The runtime container needs RBAC to read its model Secret — grant a Role/RoleBinding
for `get` on the specific Secret in the agent's namespace.

### Option B: bring your own runtime

Omit `spec.runtime` and run your process anywhere it can reach the Registry. Point
it at the Registry and identify the Agent:

```sh
export AGENTPLANE_REGISTRY=http://localhost:9090   # or the in-cluster svc
export AGENTPLANE_AGENT_NAMESPACE=<namespace>
export AGENTPLANE_AGENT_NAME=my-agent
export MODEL_API_KEY=sk-...        # or give the process Secret-read RBAC
go run ./main.go
```

---

## Recap

| Step | What you do | Where |
|---|---|---|
| Declare | Apply Model + Credential/Secret + (Prompt/Tool) + Agent CRs | `kubectl apply` |
| Verify | `PHASE=Ready`, inspect `/config` | Registry |
| Implement | Fetch config → read Secret → build tools → run loop → (hot-reload) | your runtime |
| Deploy | `spec.runtime` (operator-managed) or run it yourself | Agent CR |

**To evolve the agent, just edit the CRs.** Add a Tool, swap the Model, change the
Prompt — the Operator recomputes the config, the hash changes, and a `/watch`-based
runtime hot-reloads with no restart. That's the whole point of the split: the
declarative surface changes independently of the running data plane.

### Next steps
- [usage.md](usage.md) — full platform guide (all 14 CRDs, deploy, demo, testing).
- [runtime-protocol.md](runtime-protocol.md) — the exact wire contract your runtime
  implements (endpoints, `AgentConfig`, `configHash`, delivery guarantees).
- `cmd/example-mcp/` — a minimal MCP server if you want an MCP-backed Tool.
