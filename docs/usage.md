# CogNet Usage Guide

CogNet is a **Kubernetes-native control plane for AI Agents**. It uses the
Operator pattern to declaratively manage Agents and everything they are composed
from (Model / Tool / Skill / MCPServer / Workflow / Prompt / Memory / Policy …),
and serves the resolved configuration to runtimes through the **Registry**.

> **Positioning.** CogNet is **not** an Agent runtime. It does not do inference,
> planning/ReAct, tool-calling, or memory. It handles declaration, lifecycle,
> config aggregation, service discovery, status, and observability. The actual
> execution is done by a *runtime* (LangGraph / OpenAI Agents SDK / CrewAI / your
> own) that talks only to the Registry, never to the Kubernetes API directly.

---

## Table of contents

1. [Architecture](#1-architecture)
2. [Resource model (14 CRDs)](#2-resource-model-14-crds)
3. [Prerequisites & deploy](#3-prerequisites--deploy)
4. [Declarative usage: define an Agent in YAML](#4-declarative-usage-define-an-agent-in-yaml)
5. [Programmatic usage: create an Agent in Go](#5-programmatic-usage-create-an-agent-in-go)
6. [Data plane: config delivery & hot reload](#6-data-plane-config-delivery--hot-reload)
7. [Running a real agent (Tool/MCP calls)](#7-running-a-real-agent-toolmcp-calls)
8. [Operator-managed runtime (`spec.runtime`)](#8-operator-managed-runtime-specruntime)
9. [One-click demo](#9-one-click-demo)
10. [Verification & testing](#10-verification--testing)
11. [Tool vs Skill](#11-tool-vs-skill)
12. [FAQ](#12-faq)
13. [Repo layout](#13-repo-layout)

---

## 1. Architecture

```
        Control Plane                                Data Plane
  ┌──────────────────────────────────┐        ┌──────────────────────────┐
  │  kube-apiserver (CRDs)            │        │  Agent Runtime           │
  │        ▲ watch/reconcile          │        │  MCP Runtime / Tool      │
  │  ┌─────┴─────┐   ┌────────────┐   │        │        ▲ HTTP/SSE (config)│
  │  │ Operator  │──▶│  Registry  │◀──┼────────┼────────┘                 │
  │  │(cmd/main) │   │(cmd/registry)  │        │  runtimes never call     │
  │  └───────────┘   └────────────┘   │        │  kube-apiserver directly │
  └──────────────────────────────────┘        └──────────────────────────┘
```

- **Operator** (`cmd/main.go` + `internal/controller/`): watches all CogNet
  resources, validates, resolves references, materializes workloads (e.g.
  MCPServer → Deployment/Service, and optionally the agent runtime), publishes
  status.
- **Registry** (`cmd/registry/`): the single config source for runtimes.
  - `GET /v1/agents/{ns}/{name}/config` — one-shot resolved snapshot.
  - `GET /v1/agents/{ns}/{name}/watch` — SSE stream, pushes a fresh config on
    every Agent change (**hot reload**).
- **Admission webhooks** (`internal/webhook/`): structural validation at apply
  time (fail fast).

**Core loop:** declare resources → Operator resolves refs and computes a
`resolvedConfigHash` → writes Agent status → Registry watches Agent and pushes
via SSE → runtime compares hash and hot-reloads when it differs.

---

## 2. Resource model (14 CRDs)

API group `core.cognet.io/v1alpha1`, all Namespaced.

| Kind | Short | Purpose |
|---|---|---|
| **Agent** | ag | Declares an agent as a set of references to capabilities (core). |
| **AgentClass** | agc | Reusable defaults inherited by Agents. |
| **Model** | — | Model endpoint (openai/anthropic/azure/ollama/vllm/openrouter/custom). |
| **Tool** | — | A capability the agent **calls** (http/grpc/mcp/wasm/plugin/container). |
| **ToolSet** | ss | A named bundle of Tools. |
| **Skill** | — | A markdown instruction pack (SKILL.md-style); teaches the agent *how*; may declare `allowedTools`. |
| **MCPServer** | mcp | An MCP server; Operator materializes it into a Deployment+Service. |
| **Workflow** | wf | Engine-neutral execution shape (planner/tool/reflect/finish). |
| **PromptTemplate** | pt | Versioned system/role prompts + few-shot. |
| **Memory** | — | Memory/storage backend (redis/postgres/vector/graph/s3). |
| **KnowledgeBase** | kb | Retrieval corpus (RAG). |
| **Policy** | — | Coarse allow/deny over models/memory/mcp/tools/workflows. |
| **ToolPolicy** | tp | Fine-grained per-Tool authorization and rate limits. |
| **Credential** | cred | Indirects secret material through a K8s Secret (never inline). |

Inspect: `kubectl get crd | grep cognet`, `kubectl explain agent.spec`.

---

## 3. Prerequisites & deploy

**Tools:** Go 1.24+, Docker, kubectl, a K8s cluster (locally: OrbStack / kind /
minikube).

### 3.1 Local deploy (no cert-manager, webhooks off)

```sh
export KUBECONFIG=$HOME/.kube/config      # point at your local cluster

make docker-build IMG=cognet:dev          # local Docker shared with cluster → no push
bin/kustomize build config/local | kubectl apply -f -   # CRDs + RBAC + manager
kubectl -n agent-plane-system rollout restart deploy/agent-plane-controller-manager
kubectl -n agent-plane-system rollout status  deploy/agent-plane-controller-manager
```

`config/local` is the local overlay (no webhook/cert-manager, `ENABLE_WEBHOOKS=false`).
The full install is `config/default` (`make deploy IMG=…`, needs cert-manager).

### 3.2 In-cluster Registry (needed for in-cluster runtimes)

```sh
docker build -f Dockerfile.registry -t cognet-registry:dev .
kubectl apply -f config/registry/registry.yaml   # Deployment+Service+SA (read-only, no Secret access)
```

---

## 4. Declarative usage: define an Agent in YAML

An Agent = references. Minimal example:

```yaml
apiVersion: core.cognet.io/v1alpha1
kind: Model
metadata: {name: my-model}
spec:
  provider: openrouter
  modelName: openai/gpt-4o-mini
  credentialRef: {name: my-key}
---
apiVersion: core.cognet.io/v1alpha1
kind: Credential
metadata: {name: my-key}
spec:
  secretRef: {name: my-secret, key: api-key}   # points at a plain K8s Secret
---
apiVersion: core.cognet.io/v1alpha1
kind: Agent
metadata: {name: my-agent}
spec:
  modelRef: {name: my-model}
  # optional: promptRef / toolRefs / toolSetRefs / skillRefs / memoryRefs / policyRefs / agentClassRef / runtime
```

```sh
kubectl apply -f my-agent.yaml
kubectl get agent my-agent            # PHASE: Pending/Ready/Degraded
kubectl describe agent my-agent       # conditions + resolvedConfigHash
```

**Missing references?** The Agent becomes `Degraded`
(reason `ReferenceNotFound`) and **auto-converges** to `Ready` once the missing
resource is created (controllers watch referenced kinds). This makes apply order
irrelevant — GitOps-friendly.

A coherent sample set lives in `config/samples/`: `kubectl apply -k config/samples`.

---

## 5. Programmatic usage: create an Agent in Go

Use the typed API as a library (see `cmd/demo/main.go`):

```go
import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

_ = k.Create(ctx, &v1alpha1.Model{
    ObjectMeta: metav1.ObjectMeta{Name: "llm-model", Namespace: ns},
    Spec: v1alpha1.ModelSpec{
        Provider: "custom", ModelName: "claude-haiku-4-5-20251001",
        Endpoint: "http://gateway/v1", CredentialRef: &v1alpha1.LocalReference{Name: "llm-cred"},
    },
})
_ = k.Create(ctx, &v1alpha1.Agent{
    ObjectMeta: metav1.ObjectMeta{Name: "support-agent", Namespace: ns},
    Spec: v1alpha1.AgentSpec{
        ModelRef:  v1alpha1.LocalReference{Name: "llm-model"},
        ToolRefs:  []v1alpha1.LocalReference{{Name: "order-lookup"}},
    },
})
// then poll agent.Status.Phase == v1alpha1.AgentPhaseReady
```

Run the full code-based demo (create → wait → in-process port-forward → run
agent → clean up):

```sh
export KUBECONFIG=$HOME/.kube/config
go run ./cmd/demo "What is the status of order K-7?"
```

---

## 6. Data plane: config delivery & hot reload

Runtimes read the Registry, not Kubernetes.

```sh
make run-registry &          # or: go run ./cmd/registry   (default :9090)
curl -s localhost:9090/v1/agents/default/my-agent/config | jq
curl -N localhost:9090/v1/agents/default/my-agent/watch   # SSE; edit the Agent to see a push
```

**Hot-reload chain** (e.g. changing a Tool): change Tool → Agent controller
(watching Tools) recomputes `resolvedConfigHash` → Registry (watching Agents)
pushes a new snapshot via SSE → runtime hot-reloads. The Registry only ships the
Secret **coordinates** (name/key), never the value — the runtime reads the Secret
via its own RBAC. Full contract: `docs/runtime-protocol.md`.

---

## 7. Running a real agent (Tool/MCP calls)

`cmd/agent-runtime` is a minimal but real runtime (stands in for LangGraph et al.):
it pulls config from the Registry, reads the model key from the referenced Secret
via RBAC, reads the system prompt from the PromptTemplate, then runs a
**tool-calling loop** — `http` tools via POST, `mcp` tools via JSON-RPC
`tools/call` — feeding results back until a final answer. The reusable loop is in
`internal/agentloop/`.

```sh
make run-registry &
kubectl -n default port-forward svc/orders-mcp 18080:8080 &   # host → in-cluster MCP
go run ./cmd/agent-runtime --namespace default --name my-agent \
  --tool-endpoint order-lookup=http://localhost:18080 \
  --prompt "What is the status of order A-42?"
```

---

## 8. Operator-managed runtime (`spec.runtime`)

By default CogNet is purely declarative — you bring your own runtime and point it
at the Registry. Optionally, the Operator can **deploy the runtime for you** from
the Agent CR (pull model): set `spec.runtime` and the `AgentReconciler`
materializes an owned Deployment (+ optional Service), just like it materializes
an MCPServer.

```yaml
apiVersion: core.cognet.io/v1alpha1
kind: Agent
metadata: {name: support-agent}
spec:
  modelRef: {name: llm-model}
  toolRefs: [{name: order-lookup}]
  runtime:
    image: myorg/my-runtime:v1     # your runtime image (BYO); CogNet does not do inference
    replicas: 2
    port: 8080                     # optional → also creates a Service
    # env: [...]  resources: {...}
```

The Operator injects into each runtime pod:

| Env | Value |
|---|---|
| `COGNET_REGISTRY` | in-cluster Registry URL (default `http://cognet-registry.agent-plane-system.svc:9090`; override with the manager's `COGNET_REGISTRY_URL`) |
| `COGNET_AGENT_NAMESPACE` / `COGNET_AGENT_NAME` | which Agent to load |

The runtime container then pulls its config from the Registry and hot-reloads on
change (pull model — see the reference `--watch` mode below). The Deployment is
owned by the Agent, so deleting the Agent garbage-collects the runtime.
`status.runtimeAvailableReplicas` reflects availability.

**Reference runtime image.** `cmd/agent-runtime` has a long-running `--watch`
mode (the container default) that subscribes to the Registry, reads the Secret
via its ServiceAccount RBAC, and hot-reloads:

```sh
docker build -f Dockerfile.agent-runtime -t cognet-agent-runtime:dev .   # image defaults to --watch
# then reference it: spec.runtime.image: cognet-agent-runtime:dev
```

> The runtime pod needs RBAC to `get` Secrets in its namespace (for the model
> key). Grant its ServiceAccount a `Role`/`RoleBinding` accordingly; CogNet does
> not provision that for you.
>
> **Push vs pull:** CogNet uses **pull** (each runtime pod reconciles itself from
> the Registry). This is self-healing, handles multiple replicas and new pods,
> and avoids coupling the control plane to data-plane reachability — the same
> reasons Kubernetes itself is pull-based.

---

## 9. One-click demo

`hack/demo.sh` ties it together (detects an LLM credential, builds the MCP image,
creates resources, waits, starts Registry + port-forward, runs the agent, cleans
up):

```sh
# operator deployed; an LLM credential in env:
#   ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN   (OpenAI-compatible gateway), or
#   OPENROUTER_API_KEY
bash hack/demo.sh "What is the status of order A-42?"
KEEP=1 bash hack/demo.sh
DEMO_MODEL=claude-opus-4-6 bash hack/demo.sh
```

Pure-Go equivalent (no YAML/bash/external kubectl): `go run ./cmd/demo "..."`.

---

## 10. Verification & testing

```sh
# ① Code: envtest (real apiserver+etcd) — controller/webhook unit tests
make test

# ② Platform: live-cluster behavior (ref convergence / secret detection / validation / MCPServer GC / Tool / Skill)
export KUBECONFIG=$HOME/.kube/config
bash hack/functional-test.sh

# ③ Real agent: real model + real tool call
bash hack/demo.sh          # or: go run ./cmd/demo
```

---

## 11. Tool vs Skill

| | **Tool** (executable capability) | **Skill** (markdown instruction pack) |
|---|---|---|
| Essence | an endpoint the agent **calls** | a document that teaches the agent **how** |
| Analogy | API / function | manual / SKILL.md |
| Content | endpoint + schema + timeout | body (inline or ConfigMap) + allowedTools |
| Consumer | runtime invokes it | model reads it, decides when to use |

An Agent references Tools via `toolRefs` and Skills via `skillRefs`; both can be
used together.

---

## 12. FAQ

**kubectl hits the wrong cluster?** Many shells `export KUBECONFIG=…` in their
profile. Set it explicitly: `export KUBECONFIG=$HOME/.kube/config` (verify
`kubectl config current-context`).

**Manager CrashLoop?** Usually webhooks enabled without certs. Use `config/local`
(`ENABLE_WEBHOOKS=false`) or install cert-manager.

**MCPServer pod ImagePullBackOff?** The image doesn't exist. Replace placeholder
images with pullable ones, or use a locally-shared image (OrbStack/kind, non-`:latest`
tag so pull policy is `IfNotPresent`).

**Host can't reach in-cluster `*.svc`?** That's host↔cluster networking, not a
design flaw. In-cluster runtimes use svc DNS directly; from the host use
`kubectl port-forward` + `--tool-endpoint name=url` (`cmd/demo` port-forwards
in-process via client-go).

**`unknown field` on apply after a CRD change?** The cluster CRD is stale. Re-run
`make manifests` then `kubectl apply -k config/crd` (or `config/local`).

**Can the Registry leak secrets?** No. It only ships Secret name/key coordinates;
the value is read by the runtime via its own RBAC.

---

## 13. Repo layout

```
api/v1alpha1/           # 14 CRD types + shared types + structural validation (validation.go)
internal/controller/    # one reconciler per Kind; refutil.go = shared ref-resolution/watch helpers
internal/webhook/       # validating admission webhooks (Agent/Workflow/Tool)
internal/agentloop/     # reusable tool-calling loop (verification, not control plane)
cmd/main.go             # Operator (manager)
cmd/registry/           # Registry (data-plane config endpoint: /config, /watch)
cmd/agent-runtime/      # reference runtime (Registry-driven; one-shot + --watch modes)
cmd/demo/               # pure-Go one-click demo (typed client + in-process port-forward)
cmd/example-mcp/        # minimal MCP server (test fixture)
config/crd|rbac|manager # kustomize bases
config/default          # full deploy (webhook/cert-manager)
config/local            # local overlay (no webhook)
config/registry         # in-cluster Registry (Deployment+Service+RBAC)
config/samples          # coherent sample resources
config/demo             # a complete Agent demo's manifests
hack/demo.sh            # one-click demo script
hack/functional-test.sh # live-cluster functional test (19 checks)
```

> `cmd/agent-runtime`, `cmd/demo`, `cmd/example-mcp` are **verification fixtures**,
> not part of the control plane — they stand in for real Agent frameworks and MCP
> tool servers to prove the platform drives them end to end.
