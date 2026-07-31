# Agent Plane — Kubernetes-Native Agent Control Plane

Agent Plane is a **control plane** for AI Agents, built with the Kubernetes Operator
pattern. It manages the *declaration, lifecycle, and configuration* of Agents
and the resources they are composed from — Models, Tools, Skills, MCP servers,
Workflows, Prompts, Memory, Policies, and more — as first-class Custom
Resources.

> Agent Plane is **not** an Agent runtime. It does not do inference, planning, ReAct,
> tool-calling, or memory implementation. It declares resources, reconciles
> desired state, and serves aggregated configuration to runtimes through the
> **Registry**. Any Agent framework (LangGraph, OpenAI Agents SDK, CrewAI, …)
> can plug in as a runtime.

## Architecture

```
                Control Plane                         Data Plane
  ┌────────────────────────────────────────┐   ┌─────────────────────────┐
  │  kube-apiserver (CRDs)                 │   │  Agent Runtime          │
  │        ▲                               │   │  MCP Runtime            │
  │        │ watch/reconcile               │   │  Tool Runtime           │
  │  ┌─────┴───────┐    ┌───────────────┐  │   │        ▲                │
  │  │  Operator   │───▶│   Registry    │◀─┼───┼────────┘ HTTP (config)  │
  │  │ (this repo) │    │ (cmd/registry)│  │   │  runtimes never call    │
  │  └─────────────┘    └───────────────┘  │   │  the API server directly│
  └────────────────────────────────────────┘   └─────────────────────────┘
```

- **Operator** (`cmd/main.go`, `internal/controller/`) watches all Agent Plane
  resources, validates and resolves references, materializes workloads, and
  publishes status.
- **Registry** (`cmd/registry/`) is the single configuration source for
  runtimes. Runtimes ask the Registry for an Agent's fully-resolved config;
  they never talk to the Kubernetes API directly. It exposes:
  - `GET /v1/agents/{ns}/{name}/config` — one-shot resolved config snapshot.
  - `GET /v1/agents/{ns}/{name}/watch` — Server-Sent Events stream that pushes a
    fresh config whenever the Agent changes (**hot reload**). The Registry
    watches only Agents; the Operator folds dependency changes into the Agent's
    `resolvedConfigHash`, so an Agent event covers any change that matters.
- **Admission webhooks** (`internal/webhook/v1alpha1/`) validate *structural*
  invariants at apply time (fail fast) for Agent (no duplicate refs), Workflow
  (unique step names, no dangling `next`), and Tool (an `mcp` tool needs an
  `mcpServerRef`). The same checks live in `api/v1alpha1/validation.go` and are
  reused by the controllers — cross-object *existence* is deliberately left to
  the controllers' eventual consistency so GitOps can apply in any order.

## Resource model (`core.hkmdxlftjf.io/v1alpha1`)

| Kind | Purpose |
|------|---------|
| **Agent** | Declares an agent as references to the capabilities it is built from. |
| **AgentClass** | Reusable defaults inherited by Agents. |
| **Model** | A model endpoint (openai/anthropic/azure/ollama/vllm/openrouter/custom). |
| **Tool** | A single executable capability the agent *calls* (http/grpc/mcp/wasm/plugin/container). |
| **ToolSet** | A named bundle of Tools. |
| **Skill** | A markdown instruction pack (SKILL.md-style) that teaches the agent *how* to do something; its `allowedTools` confine the agent once the skill is loaded. |
| **MCPServer** | An MCP server; the Operator materializes it into a Deployment + Service. |
| **Workflow** | Engine-neutral execution shape (planner/tool/reflect/finish). |
| **PromptTemplate** | Versioned system/role prompts and few-shot examples. |
| **Memory** | A memory/storage backend (redis/postgres/vector/graph/s3). |
| **KnowledgeBase** | A retrieval corpus (RAG). |
| **Policy** | Coarse allow/deny over models/memory/mcp/tools/workflows. Enforced: the Operator refuses to run an Agent whose refs are denied. |
| **ToolPolicy** | Per-Tool authorization and per-session call caps. Enforced by the runtime at call time. |
| **Credential** | Indirects secret material through a Kubernetes Secret. |

### The two reference controllers

- **`AgentReconciler`** — the *aggregating* pattern. Resolves every reference an
  Agent declares; if any is missing, marks the Agent `Degraded` with reason
  `ReferenceNotFound`. If everything exists but a referenced **Policy** forbids
  one of them, marks it `Degraded` with reason `PolicyViolation` — reported on a
  separate `PolicyCompliant` condition, so "the Model is missing" stays
  distinguishable from "the Model exists but this Agent may not use it".
  Otherwise computes a stable `resolvedConfigHash` over all resolved references
  (so runtimes/Registry can detect drift) and marks the Agent `Ready`. Watches
  all referenceable kinds so a change to a dependency re-reconciles the Agents
  that use it.
- **`MCPServerReconciler`** — the *resource-owning* pattern. Creates and owns a
  `Deployment` + `Service` for each MCPServer via `CreateOrUpdate` +
  `SetControllerReference`, and reflects availability into status.

The remaining 11 controllers follow the same two shapes: **reference-resolving**
(Model, Memory, Tool, ToolSet, Skill, KnowledgeBase, AgentClass, Credential) validate
and resolve what they point at, and **validation-only** (Workflow, ToolPolicy,
Policy, PromptTemplate) check internal consistency. Each watches the kinds it
depends on, so the reference graph converges automatically — a resource stuck
`Degraded` on a missing dependency flips to `Ready` as soon as that dependency
is created.

Those watches are **reference-precise**: field indexes (`SetupFieldIndexes`) map
a changed dependency back to just the resources naming it, rather than every
resource in the namespace. Two indirect routes are followed explicitly, since
missing them would drop events rather than merely waste work — an Agent
inheriting a `modelRef` from its AgentClass, and a Tool reaching an Agent
through a ToolSet.

Policy and ToolPolicy have no workload of their own, so their controllers only
publish readiness — but the rules are *not* inert. `AgentReconciler` merges the
policies each Agent references and refuses to run it if its declaration is
forbidden; the Registry ships the same merged view to runtimes, which enforce the
call-time half. See **[docs/usage.md](docs/usage.md)** §12.

## Getting started

Prerequisites: Go 1.24+, Docker, `kubectl`, and access to a cluster
(e.g. `kind`).

```sh
# Generate code + CRDs (already committed, but safe to re-run)
make generate manifests

# Compile everything
make build            # operator -> bin/manager
make build-registry   # registry -> bin/registry

# Install CRDs and run the operator against your current kube context
make install
make run

# In another shell, apply the coherent sample set
kubectl apply -k config/samples

# Watch the Agent resolve its references and the MCPServer spawn a Deployment
kubectl get agents,mcpservers
kubectl get deploy,svc -l app.kubernetes.io/managed-by=agent-plane

# Serve resolved config to runtimes and fetch one Agent's config
make run-registry &
curl localhost:9090/v1/agents/default/support-agent/config | jq

# Stream hot-reload updates for one Agent (edit the Agent in another shell to see a push)
curl -N localhost:9090/v1/agents/default/support-agent/watch
```

> Webhooks are wired into the manager and run by default; set
> `ENABLE_WEBHOOKS=false` to disable them when running locally without certs.
> In-cluster they require cert-manager (uncomment the `webhook`/`cert-manager`
> entries in `config/default/kustomization.yaml`).

## Reference runtime: a real agent driven by Agent Plane

`cmd/agent-runtime` is a minimal *real* agent that stands in for a full Agent
framework, built on the Go SDK
([`github.com/hkmdxlftjf/agent-plane-sdk-go`](https://github.com/hkmdxlftjf/agent-plane-sdk-go)).
Point it at a deployed Agent and it:

1. pulls the resolved config from the **Registry** (never the API server),
2. reads the model API key from the referenced **Secret** via its own RBAC,
3. takes the Registry-resolved **PromptTemplate** system prompt and advertises any
   **Skill** as a name + description catalog — full instruction bodies load on demand
   via a built-in `load_skill` tool, so the prompt stays flat however many skills an
   Agent mounts — restores conversation **Memory**, and retrieves from
   **KnowledgeBases**, and
4. runs a **tool-calling loop**: the model requests a tool, the runtime invokes
   it over **MCP JSON-RPC** (or plain HTTP), feeds the result back, and returns
   the answer — refusing any call its **Policy**/**ToolPolicy** forbids, and
   handing the reason to the model rather than failing the run.

```sh
# operator + in-cluster Registry deployed first; needs an LLM key in the Model's Secret.
go run ./cmd/agent-runtime --namespace <ns> --name <agent> --prompt "What is the status of order A-42?"
```

Sample output:

```
[step 1] model calls tool order-lookup({"orderId":"A-42"})
         ↳ result: {"carrier":"UPS","eta":"2026-07-20","status":"shipped",...}
✅ Final answer:
Order A-42 is shipped via UPS with an ETA of July 20, 2026.
```

`agent-runtime` and `example-mcp` are **test fixtures**, not part of the control
plane — they stand in for a real Agent framework and a real MCP tool server to
prove the platform drives them end-to-end. See
**[docs/quickstart-custom-agent.md](docs/quickstart-custom-agent.md)** for a full
walkthrough of declaring, implementing, and deploying your own agent.

## Operator-managed runtime (`spec.runtime`)

By default Agent Plane is purely declarative: you bring your own runtime and point it
at the Registry. Optionally, the Operator can **deploy the runtime for you** from
the Agent CR (pull model) — set `spec.runtime` and the `AgentReconciler`
materializes an owned Deployment (+ optional Service), the same way it
materializes an `MCPServer`:

```yaml
spec:
  modelRef: {name: llm-model}
  runtime:
    image: myorg/my-runtime:v1     # your runtime image (BYO); Agent Plane does not do inference
    replicas: 2
    port: 8080                     # optional → also creates a Service
```

The Operator injects `AGENTPLANE_REGISTRY`, `AGENTPLANE_AGENT_NAMESPACE`, and
`AGENTPLANE_AGENT_NAME`; the runtime container pulls its config from the in-cluster
Registry and hot-reloads on change. The reference image (`Dockerfile.agent-runtime`,
`cmd/agent-runtime --watch`) does exactly this. In-cluster Registry manifests are
in `config/registry/`. See **[docs/usage.md](docs/usage.md)** §8 and
**[docs/runtime-protocol.md](docs/runtime-protocol.md)** for the full contract.

## Documentation

- **[docs/quickstart-custom-agent.md](docs/quickstart-custom-agent.md)** — zero-to-running
  guide: declare a custom agent, implement its runtime code, and deploy it.
- **[docs/usage.md](docs/usage.md)** — full usage guide (deploy, declarative &
  programmatic usage, data plane, runtime, FAQ).
- **[docs/runtime-protocol.md](docs/runtime-protocol.md)** — the runtime
  configuration & change-notification protocol (v1).

## Roadmap (out of scope for this scaffold)

- Registry gRPC transport and event-bus fan-out (HTTP + SSE are implemented)
- Defaulting / conversion webhooks (validating webhooks are implemented)
- Narrowing the manager's Secret/ConfigMap *cache* (enqueue is already
  reference-precise; the informer still watches them cluster-wide)
- Multi-tenant scoping (Namespace / Cluster / Tenant)
- Metrics dashboards and tracing wiring
- Reference runtime integrations (LangGraph, Agents SDK, CrewAI)

## License

Apache 2.0.
