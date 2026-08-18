# Agent Plane Usage Guide

Agent Plane is a **Kubernetes-native control plane for AI Agents**. It uses the
Operator pattern to declaratively manage Agents and everything they are composed
from (Model / Tool / Skill / MCPServer / Workflow / Prompt / Memory / Policy …),
and serves the resolved configuration to runtimes through the **Registry**.

> **Positioning.** Agent Plane is **not** an Agent runtime. It does not do inference,
> planning/ReAct, tool-calling, or memory. It handles declaration, lifecycle,
> config aggregation, service discovery, status, and observability. The actual
> execution is done by a *runtime* (LangGraph / OpenAI Agents SDK / CrewAI / your
> own) that talks only to the Registry, never to the Kubernetes API directly.

---

## Table of contents

1. [Architecture](#1-architecture)
2. [Resource model (15 CRDs)](#2-resource-model-15-crds)
3. [Prerequisites & deploy](#3-prerequisites--deploy)
4. [Declarative usage: define an Agent in YAML](#4-declarative-usage-define-an-agent-in-yaml)
5. [Programmatic usage: create an Agent in Go](#5-programmatic-usage-create-an-agent-in-go)
6. [Data plane: config delivery & hot reload](#6-data-plane-config-delivery--hot-reload)
7. [Running a real agent (Tool/MCP calls)](#7-running-a-real-agent-toolmcp-calls)
8. [Operator-managed runtime (`spec.runtime`)](#8-operator-managed-runtime-specruntime)
9. [Running the reference runtime](#9-running-the-reference-runtime)
10. [Verification & testing](#10-verification--testing)
11. [Tool vs Skill](#11-tool-vs-skill)
12. [Authorization: Policy, ToolPolicy, and skill scope](#12-authorization-policy-toolpolicy-and-skill-scope)
13. [Inbound: IM bots and other event sources](#13-inbound-im-bots-and-other-event-sources)
14. [Coding agents: one repo per Agent](#14-coding-agents-one-repo-per-agent)
15. [FAQ](#15-faq)
16. [Repo layout](#16-repo-layout)

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

- **Operator** (`cmd/main.go` + `internal/controller/`): watches all Agent Plane
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

**Dependency watches are reference-precise.** Changing a Model re-reconciles only
the Agents that name it, via field indexes registered in
`controller.SetupFieldIndexes` before the controllers start. If you add a
reference field to a spec, add an index for it in `internal/controller/fieldindex.go`
and wire the watch to it — a watch with no matching index silently stops
re-reconciling, which looks like a resource that just never converges.

Two routes reach a resource without it naming the dependency directly, and both
are handled: an Agent inheriting `modelRef`/`workflowRef`/`promptRef`/`policyRefs`
from its AgentClass, and a Tool reaching an Agent through a ToolSet. Add a third
such route and the mapper needs to know about it.

---

## 2. Resource model (15 CRDs)

API group `core.hkmdxlftjf.io/v1alpha1`, all Namespaced.

| Kind | Short | Purpose |
|---|---|---|
| **Agent** | ag | Declares an agent as a set of references to capabilities (core). |
| **AgentClass** | agc | Reusable defaults inherited by Agents. |
| **Model** | — | Model endpoint (openai/anthropic/azure/ollama/vllm/openrouter/custom). |
| **Tool** | — | A capability the agent **calls**: `http`, or `mcp` backed by an MCPServer or a peer Agent (see §14). |
| **ToolSet** | ss | A named bundle of Tools. |
| **Skill** | — | A markdown instruction pack (SKILL.md-style); teaches the agent *how*; its `allowedTools` confine tool calls once loaded (see §12). |
| **MCPServer** | mcp | An MCP server; Operator materializes it into a Deployment+Service. |
| **Workflow** | wf | Engine-neutral execution shape (planner/tool/reflect/finish). |
| **PromptTemplate** | pt | Versioned system/role prompts + few-shot. |
| **Memory** | — | Memory/storage backend (redis/postgres/vector/graph/s3). |
| **KnowledgeBase** | kb | Retrieval corpus (RAG). |
| **Policy** | — | Coarse allow/deny over models/memory/mcp/tools/workflows; enforced (see §12). |
| **ToolPolicy** | tp | Per-Tool allow/deny and per-session call caps; enforced (see §12). |
| **Credential** | cred | Indirects secret material through a K8s Secret (never inline). |
| **Trigger** | trg | An inbound event source (IM bot, webhook) feeding an Agent; runs a BYO adapter image (see §13). |

Inspect: `kubectl get crd | grep agent-plane`, `kubectl explain agent.spec`.

---

## 3. Prerequisites & deploy

**Tools:** Go 1.24+, Docker, kubectl, a K8s cluster (locally: OrbStack / kind /
minikube).

### 3.1 Local deploy (no cert-manager, webhooks off)

```sh
export KUBECONFIG=$HOME/.kube/config      # point at your local cluster

make docker-build IMG=agent-plane:dev          # local Docker shared with cluster → no push
bin/kustomize build config/local | kubectl apply -f -   # CRDs + RBAC + manager
kubectl -n agent-plane-system rollout restart deploy/agent-plane-controller-manager
kubectl -n agent-plane-system rollout status  deploy/agent-plane-controller-manager
```

`config/local` is the local overlay (no webhook/cert-manager, `ENABLE_WEBHOOKS=false`).
The full install is `config/default` (`make deploy IMG=…`, needs cert-manager).

### 3.2 In-cluster Registry (needed for in-cluster runtimes)

```sh
docker build -f Dockerfile.registry -t agent-plane-registry:dev .
kubectl apply -f config/registry/registry.yaml   # Deployment+Service+SA (read-only, no Secret access)
```

---

## 4. Declarative usage: define an Agent in YAML

An Agent = references. Minimal example:

```yaml
apiVersion: core.hkmdxlftjf.io/v1alpha1
kind: Model
metadata: {name: my-model}
spec:
  provider: openrouter
  modelName: openai/gpt-4o-mini
  credentialRef: {name: my-key}
---
apiVersion: core.hkmdxlftjf.io/v1alpha1
kind: Credential
metadata: {name: my-key}
spec:
  secretRef: {name: my-secret, key: api-key}   # points at a plain K8s Secret
---
apiVersion: core.hkmdxlftjf.io/v1alpha1
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

Use the typed API as a library:

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
        ModelRef:  &v1alpha1.LocalReference{Name: "llm-model"},
        ToolRefs:  []v1alpha1.LocalReference{{Name: "order-lookup"}},
    },
})
// then poll agent.Status.Phase == v1alpha1.AgentPhaseReady
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
via RBAC (the system prompt arrives already resolved in the config), then runs a
**tool-calling loop** — `http` tools via POST, `mcp` tools via JSON-RPC
`tools/call` — feeding results back until a final answer. It is built entirely on
the Go SDK, [`github.com/hkmdxlftjf/agent-plane-sdk-go`](https://github.com/hkmdxlftjf/agent-plane-sdk-go);
the reusable loop is the SDK's `agentloop` package.

```sh
make run-registry &
kubectl -n default port-forward svc/orders-mcp 18080:8080 &   # host → in-cluster MCP
go run ./cmd/agent-runtime --namespace default --name my-agent \
  --tool-endpoint order-lookup=http://localhost:18080 \
  --prompt "What is the status of order A-42?"
```

---

## 8. Operator-managed runtime (`spec.runtime`)

By default Agent Plane is purely declarative — you bring your own runtime and point it
at the Registry. Optionally, the Operator can **deploy the runtime for you** from
the Agent CR (pull model): set `spec.runtime` and the `AgentReconciler`
materializes an owned Deployment (+ optional Service), just like it materializes
an MCPServer.

```yaml
apiVersion: core.hkmdxlftjf.io/v1alpha1
kind: Agent
metadata: {name: support-agent}
spec:
  modelRef: {name: llm-model}
  toolRefs: [{name: order-lookup}]
  runtime:
    image: myorg/my-runtime:v1     # your runtime image (BYO); Agent Plane does not do inference
    replicas: 2
    port: 8080                     # optional → also creates a Service
    # env: [...]  resources: {...}
    # readinessProbe: {...}         # optional → see "Readiness" below
    # runtimeClassName: gvisor      # optional → schedule onto a sandboxed runtime
```

**Readiness.** For a plain runtime, no probe is set: an Agent that serves
`/api/chat` is ready as soon as it listens. Workspace-bound runtimes get a
default probe instead, because for them listening and working are not the same
thing — see §14.

**`runtimeClassName`** picks a container runtime for the pod, e.g. a
syscall-intercepting sandbox such as gVisor or Kata. The pod-level isolation the
Operator already applies to workspace runtimes (non-root, no capabilities,
read-only root filesystem) is enforced by the host kernel; a sandboxed runtime
additionally raises the cost of escaping it, which is worth paying for when the
Agent executes model-directed shell commands. The named RuntimeClass must exist
in the cluster already — naming one that does not leaves pods `Pending` with no
other symptom. Clearing the field moves the Agent back to the default runtime on
the next rollout.

The Operator injects into each runtime pod:

| Env | Value |
|---|---|
| `AGENTPLANE_REGISTRY` | in-cluster Registry URL (default `http://agent-plane-registry.agent-plane-system.svc:9090`; override with the manager's `AGENTPLANE_REGISTRY_URL`) |
| `AGENTPLANE_AGENT_NAMESPACE` / `AGENTPLANE_AGENT_NAME` | which Agent to load |
| `AGENTPLANE_CREDENTIALS_PATH` | when `spec.credentialRefs` is set — see _Credentials_ below |

The runtime container then pulls its config from the Registry and hot-reloads on
change (pull model — see the reference `--watch` mode below). The Deployment is
owned by the Agent, so deleting the Agent garbage-collects the runtime.
`status.runtimeAvailableReplicas` reflects availability.

### Volumes (`spec.runtime.volumes`)

An Agent reaches storage the control plane knows nothing about — a NAS share, a
ConfigMap of settings, a scratch disk — by declaring it:

```yaml
spec:
  runtime:
    image: myorg/assistant:v1
    volumes:
      - name: nas
        mountPath: /mnt/nas
        readOnly: true
        persistentVolumeClaim: {claimName: nas-share}
      - name: settings
        mountPath: /etc/assistant
        configMap: {name: assistant-config}
      - name: scratch
        mountPath: /scratch
        emptyDir: {sizeLimit: 1Gi}
```

Volume and mount are one entry, not two lists matched by name: an Agent volume
goes into exactly one container, so the split would only allow the mistake it
exists to permit.

**The sources are a subset of the pod API, and `hostPath` is not in it.** A
runtime executing model-directed shell commands runs under a read-only root
filesystem, dropped capabilities and a non-root uid; one `hostPath` mount steps
around all of it. An external filesystem belongs behind a PersistentVolumeClaim
— bind NFS/CIFS/a cloud disk with the cluster's usual storage machinery, and its
credentials stay in the PersistentVolume rather than in an Agent that anyone with
namespace read access can see. Inline `nfs` and `csi` sources were left out for
the same reason.

**Mounting volumes also sandboxes the container** (non-root, no capabilities,
read-only root filesystem), on the same argument as §14: the runtime runs
commands a model chose. The single-replica pinning is *not* applied — that
follows from a workspace's ReadWriteOnce claim, and would silently cap an Agent
whose own volumes are read-only or ReadWriteMany.

A volume may not take a name or mount path the Operator manages (`workspace`,
`tmp`, `git-credential`, `model-credential`, the `credential-` prefix, `/tmp`,
`/var/run/agentplane/*`, or `spec.workspace.mountPath`). That is rejected at
apply time rather than resolved silently, because a shadowing mount fails as an
empty checkout or a wrong token — a long way from the declaration at fault.

### Credentials (`spec.credentialRefs`)

For the things Agent Plane does not model — an IM app secret, a home automation
token, a vendor API key:

```yaml
spec:
  credentialRefs:
    - name: lark-app
    - name: vendor-api
```

Each Credential's Secret is mounted read-only at
`$AGENTPLANE_CREDENTIALS_PATH/<credential-name>/`, one file per key — so the
example above yields `/var/run/agentplane/credentials/lark-app/app-id` and
`…/lark-app/app-secret`. A subdirectory each, unlike the Trigger's single flat
directory (§13), because an Agent may hold several credentials whose key names
collide.

> **Mounting does not hide the secret from the model.** It keeps the value out
> of `kubectl describe pod`, out of the process environment, and out of the
> children the agent spawns — which is where secrets leak by accident. But a
> runtime that executes model-directed shell commands can read the file, exactly
> as it could read an environment variable. This narrows accidental exposure; it
> is not a boundary against the agent itself. An Agent that must *not* hold a
> credential should reach the capability through a `Tool` whose MCP server holds
> it instead — that is what `config/samples/coding/lark-mcp.yaml` does.

A missing Credential leaves the Agent `Degraded` with `ReferenceNotFound` and
converges when it appears, like any other reference.

**Reference runtime image.** `cmd/agent-runtime` has a long-running `--watch`
mode (the container default) that subscribes to the Registry, reads the Secret
via its ServiceAccount RBAC, and hot-reloads:

```sh
docker build -f Dockerfile.agent-runtime -t agent-plane-runtime:dev .   # image defaults to --watch
# then reference it: spec.runtime.image: agent-plane-runtime:dev
```

> The runtime pod needs RBAC to `get` Secrets in its namespace (for the model
> key). Grant its ServiceAccount a `Role`/`RoleBinding` accordingly; Agent Plane does
> not provision that for you.
>
> **Push vs pull:** Agent Plane uses **pull** (each runtime pod reconciles itself from
> the Registry). This is self-healing, handles multiple replicas and new pods,
> and avoids coupling the control plane to data-plane reachability — the same
> reasons Kubernetes itself is pull-based.

---

## 9. Running the reference runtime

`cmd/agent-runtime` is a minimal real agent that pulls config from the Registry,
reads the model key from the Secret, exposes Skills as a load-on-demand catalog,
folds in Memory / KnowledgeBase context, and runs a tool-calling loop. Point it at
a deployed Agent:

```sh
export KUBECONFIG=$HOME/.kube/config
# with an in-cluster Registry, port-forward it (or run `go run ./cmd/registry`):
kubectl -n agent-plane-system port-forward svc/agent-plane-registry 9090:9090 &

# one-shot:
go run ./cmd/agent-runtime --registry http://localhost:9090 \
  --namespace <ns> --name <agent> --prompt "What is the status of order A-42?"
# interactive REPL:      add --chat
# browser chat UI + API: add --serve   (or set AGENTPLANE_SERVE=1)
# long-running hot-reload (the deployed default): add --watch
```

See **[quickstart-custom-agent.md](quickstart-custom-agent.md)** for the full
declare → implement → deploy walkthrough.

---

## 10. Verification & testing

```sh
# ① Code: envtest (real apiserver+etcd) — controller/webhook unit tests + agentmemory
make test

# ② Real agent: real model + real tool call (see §9)
go run ./cmd/agent-runtime --namespace <ns> --name <agent> --prompt "..."
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

In the reference runtime a Skill is *progressively disclosed*: the system prompt
carries only a `# Skills available` catalog of `name: description` lines, and the
model calls the built-in `load_skill(name)` tool to pull a body into context when
a task actually needs it. So mounting ten skills costs ten catalog lines per turn
rather than ten full instruction packs, at the price of one extra tool-calling turn
whenever a skill is used (hence the runtime's `--max-steps` default of 8).

Loading a skill can also *narrow* what the agent may call. If a Skill declares
`allowedTools`, then from the moment its body is disclosed the session's tool
calls are confined to the union of the loaded skills' `allowedTools` — the model
has been told how to do something, and that instruction carries the tools it may
use. A Skill declaring none restricts nothing, which is the common case. See §12.

---

## 12. Authorization: Policy, ToolPolicy, and skill scope

`Policy` and `ToolPolicy` are enforced in **two places**, because neither place
can do the whole job alone.

**Control plane (fail fast).** When an Agent's declared references are denied by
a Policy it references, `AgentReconciler` marks it `Degraded` with reason
`PolicyViolation` and clears `resolvedConfigHash` — so no runtime ever fetches a
config for it. Everything the Agent *declares* is checked: `modelRef`,
`workflowRef`, `memoryRefs`, `toolRefs`, the tools contributed by every
`toolSetRefs` member, and the MCPServer behind each tool (so a ToolSet is not a
way to smuggle a denied tool past policy). The result is reported on its own
condition, separate from `Resolved`:

```sh
kubectl get agent my-agent -o jsonpath='{.status.conditions}' | jq
# Resolved:        True   — every reference exists
# PolicyCompliant: False  — …but a Policy forbids one of them
# Ready:           False  (reason: PolicyViolation)
```

The distinction matters when debugging: `ReferenceNotFound` means "the Model does
not exist", `PolicyViolation` means "it exists but this Agent may not use it".

The same gate catches an incoherent Skill: if a Skill's `allowedTools` names a
Tool the Agent does not reference, the Agent is refused here rather than failing
mid-conversation with a puzzling refusal.

**Runtime (call time).** Which tool the model reaches for on a given turn, how
often, and which Skills it has loaded, are invisible to the control plane. The
Registry therefore ships the merged policy in the config's `policy` field and the
runtime enforces it. In the reference runtime that is one line — the SDK's
`policy` package supplies a guard matching `agentloop.Config.ToolGuard`:

```go
enf := policy.New(cfg.Policy)               // nil-safe: no policy => allow all
sess := enf.Session()                       // one Session per conversation
base.ToolGuard = sess.Guard
```

A refused call is **not** a failed run: the reason goes back to the model as the
tool result, so it can explain itself or choose another route rather than the
request erroring out. `maxCallsPerSession` budgets are per conversation, so each
chat gets its own; the runtime logs its effective policy at startup and on every
hot reload.

Merging is one-directional by design: **allow lists intersect, deny lists union,
and deny always beats allow**, so attaching another Policy can only ever narrow
what an Agent may do. An empty `allow` list means "allow what is not denied", not
"allow nothing".

### Skill scope (`allowedTools`)

A third, *dynamic* dimension sits inside the session. When the model calls
`load_skill`, the runtime reports the disclosure to the enforcer:

```go
sess.NoteSkillLoaded(skill.Name, skill.AllowedTools)
```

From then on the session's tool calls are confined to the union of the loaded
skills' `allowedTools`. Three properties are worth knowing:

- **A skill declaring no `allowedTools` restricts nothing.** "No restriction" and
  "restricted to nothing" are different states; the scope stays absent until a
  restricting skill loads.
- **A skill can only narrow.** A tool a Policy denies stays denied even if a
  loaded skill lists it — otherwise writing a Skill would be a way to escalate
  past a Policy.
- **Loads union, they don't intersect.** Two instruction packs each legitimately
  need their own tools, so loading a second skill never breaks the first.

See `config/samples/travel/policy.yaml` for a worked example.

---

## 13. Inbound: IM bots and other event sources

Everything above is *outbound*: the Agent calls Tools. Receiving messages — a
Lark or DingTalk bot, a webhook — is the other direction, and it is a `Trigger`.

Agent Plane implements no platform protocol. You supply an adapter image; the
Operator schedules it, mounts its credentials, and tells it where the Agent is:

```yaml
kind: Trigger
spec:
  agentRef: {name: support-agent}
  image: myorg/lark-adapter:v1       # swap this for DingTalk, Slack, …
  credentialRef: {name: lark-app}
  config: {events: ["im.message.receive_v1"]}
```

The adapter does three things: connect to the platform, `POST
$AGENTPLANE_AGENT_ENDPOINT/api/chat` with `{sessionId, message}`, and post the
answer back. `sessionId` should be the platform's conversation id — the runtime
keys memory on it, so a stable id gives multi-turn context for free and a shared
one leaks history between chats.

**Adding a platform is a new image and a new Trigger**, not a control-plane
change. That is the whole point of fixing the contract instead of building
integrations in.

Two requirements worth stating plainly:

- The Agent needs `spec.runtime` **with a port** — that is what creates the
  Service the adapter POSTs to. Without it the Trigger reports `Degraded` saying
  so, rather than injecting an address nothing listens on.
- `replicas` is capped at 1. Most platforms deliver each event to every open
  connection, so a second replica answers every message twice.

`status.phase: Running` means the adapter pod is up — not that it authenticated
with the platform, which only the adapter can know.

Full contract: **[docs/adapter-protocol.md](adapter-protocol.md)**. Worked
example: `config/samples/inbound/lark-trigger.yaml`.

---

## 14. Coding agents: one repo per Agent

A coding agent — Claude Code, Codex, OpenCode — needs a working tree, and a
working tree has exactly one writer. So the unit is **one Agent, one repository,
one pod**:

```yaml
kind: Agent
metadata: {name: api-agent}
spec:
  agentClassRef: {name: backend-role}   # model + prompt + guardrails
  workspace:
    repository: https://github.com/org/api
    branch: main
    credentialRef: {name: git-token}
  runtime:
    image: your-coding-agent:latest
    port: 8080
```

> **`spec.workspace` is a persistent working directory; the repository is
> optional.** Everything it carries — a provisioned volume, a writable durable
> `HOME` on it, and the sandbox described below — is useful to any agent that
> keeps state, not just one holding a checkout. Omit `repository` and the
> directory simply starts empty:
>
> ```yaml
> spec:
>   workspace: {size: 5Gi}     # a durable scratch directory, no git
>   runtime: {image: myorg/assistant:v1}
> ```
>
> `branch` and `credentialRef` only describe a clone, so setting either without
> a `repository` is rejected at apply time rather than silently ignored. The
> rest of this section is about the repository-bound case.

The Operator provisions a PersistentVolumeClaim, clones into it with an init
container, and mounts it at `spec.workspace.mountPath` (default `/workspace`).
The checkout, its branches, and any build cache survive pod restarts — the clone
step fetches and resets an existing tree rather than re-cloning it.

Two consequences worth stating plainly:

- **The Deployment is pinned to one replica with the `Recreate` strategy.** This
  is not tunable while a workspace is set. `RollingUpdate` would start a second
  pod on the same checkout before the first exited, and with `ReadWriteOnce` it
  could not even schedule.
- **The git credential is mounted, never interpolated into the remote URL.** A
  URL-embedded token leaks into `git remote -v`, the reflog, and any error git
  prints. The clone step reads it through a credential helper.

### Roles are separate from repositories

`promptRef` gives an Agent its role; `AgentClass` makes a role reusable. Ten
repositories across four roles is four AgentClasses and ten short Agents, not
forty full configurations — and a class carries `defaultToolRefs`,
`defaultSkillRefs`, and `defaultToolPolicyRefs`, so a role's tools and guardrails
travel with it.

Inheritance **fills gaps, it does not merge**: an Agent that sets `policyRefs`
itself gets none of the class's `defaultPolicyRefs`. To extend a class's list,
restate it.

### Cross-repository work is a declared edge

An Agent that should answer others sets `spec.expose`; the Operator publishes a
peer Service and records the address in `status.peerEndpoint`. Another Agent
reaches it through an ordinary Tool:

```yaml
kind: Agent
metadata: {name: web-agent}
spec:
  expose:
    description: Owns the web frontend. Ask about component APIs.
---
kind: Tool
metadata: {name: ask-web-agent}
spec:
  type: mcp
  agentRef: {name: web-agent}    # instead of mcpServerRef
---
kind: Agent
metadata: {name: api-agent}
spec:
  toolRefs: [{name: ask-web-agent}]
```

**Because the edge is a Tool, the authorization you already have governs it.**
Denying `ask-web-agent` in a ToolPolicy severs the link; `maxCallsPerSession`
caps how often one agent may interrupt another. There is no separate
agent-to-agent permission model to keep in sync — which is the reason peering
was built this way rather than as its own CRD.

**Isolation is the default.** An Agent with no peer Tool cannot reach any other
Agent. The topology is whatever the Tools declare, and `kubectl get agent -o yaml`
shows it.

Two declaration errors are refused at reconcile rather than surfacing as a tool
that always fails: a peer Tool naming the Agent itself, and one naming an Agent
that does not set `spec.expose`. A peer that is merely *unhealthy* is not a
violation — failing the caller for it would let one broken repository cascade
across every agent that consults it.

Worked example: `config/samples/coding/repo-agents.yaml`.

### Adapting a coding agent

Coding agents are not HTTP servers and accept no injected system prompt or tool
list, so the way to run one here is **projection**: write the Registry's config
into whatever the agent already reads, without patching the agent itself.

Where that projection *runs* depends on the agent. Two shapes:

**A plugin, in-process** — the shape `Dockerfile.coding-agent` uses, with
opencode. opencode loads plugins from `.opencode/plugin` and gives them a
`config` hook that fires while the plugin loads, whose mutations reach the live
server. So a plugin can fetch the Registry, inject the model provider,
credentials, MCP servers and permissions, and be finished before the first turn.
No sidecar, no `/api/chat` translation, and the inbound platform connection can
live in the same process (§Talking to it from an IM client).

> **The plugin lives in its own repository, and must be published before the
> image can be built anywhere but a laptop.** `Dockerfile.coding-agent` installs
> it by version; with nothing published, that install 404s and the image build
> fails in CI while succeeding locally, because a local build passes
> `--build-arg PLUGIN_TARBALL=…` and points npm at a tarball instead. Publish the
> version, then build — the `PLUGIN_TARBALL` path is for testing a change before
> publishing it, not for releases.

**A shell, out-of-process** — for a CLI with no plugin system. The shell writes
the config files, serves `POST /api/chat` (§runtime protocol), execs the CLI per
turn, and maps the caller's `sessionId` to the CLI's own session so follow-ups
resume rather than start cold. Serialize turns: one working tree, one writer.

Whichever shape, three things are worth knowing before you build on it:

- **A writable, durable `HOME` is required, and where it points matters.** A
  workspace pod's root filesystem is read-only, and coding agents keep real state
  (session history, caches, resolved plugin dependencies). The Operator points
  `HOME` at a directory on the working tree's volume, so that state survives a
  restart — a conversation resumes after the pod is rescheduled instead of
  starting over. `/tmp` is writable too, but it is an emptyDir: every restart
  would forget everything.
- **Anything the agent fetches on first run must be baked into the image.** A
  cluster with no egress to npm or a model catalog will *hang* during startup
  rather than fail, which is much harder to diagnose than an error.
  `Dockerfile.coding-agent` pre-installs the plugin's dependencies and the model
  catalog for exactly this reason.
- **`1/1 Running` is not evidence that it works, so a workspace runtime gets a
  readiness probe by default.** A projecting runtime binds its port and answers a
  health endpoint *before* it has fetched anything, so a failed projection — an
  unreachable Registry, a plugin that did not load, a config hook that threw —
  looks exactly like a healthy pod while every request hangs. The default probe
  therefore does not check liveness; it asks for the runtime's own config and
  requires the projected model to be in it, which is only true once projection
  succeeded. Two things follow. A slow first start is not a failure, so
  `failureThreshold` is deliberately generous (a cold volume may spend minutes
  fetching a model catalog). And because opencode reads config only at startup,
  a pod that failed to project **never recovers on its own** — restoring the
  Registry does not fix it; the pod has to be replaced. Readiness staying false
  is the correct report of that, not an over-strict probe. Override with
  `spec.runtime.readinessProbe` for a runtime that exposes a better signal.
- **ToolPolicy governs declared Tools, not the agent's built-ins.** `bash`,
  `edit`, and friends are not Tool CRs, so a ToolPolicy says nothing about them,
  and `maxCallsPerSession` has no equivalent. Neither does a Skill's
  `allowedTools`, nor a `type: http` Tool. **Log each unenforceable rule at
  startup** rather than letting a policy look applied when half of it is inert.

**Permission prompts do have someone to answer them now.** This used to be the
warning here: a CLI that asks before writing finds no human at a terminal and
either denies silently or stalls. With the plugin shape the asker and the
answerer are wired together — opencode blocks the turn and emits a permission
request, the plugin renders it into the chat, and the user's answer releases the
turn. The model's work up to the question is resumed, not redone. Skills are
similar: opencode reads them from a skill directory on demand, so the
progressive-disclosure property §11 describes survives projection.

### Talking to it from an IM client

For a plain Agent, a `Trigger` (§13) brings messages in and the adapter contract
holds unchanged.

For a **workspace** Agent running the plugin shape, there is no Trigger and no
adapter pod: the plugin holds the platform connection itself, inside the pod that
runs the model. That is what makes the approval loop work end to end — the thing
that blocks the turn and the thing that shows the user a button are the same
process.

The tradeoff is explicit. One moving part instead of two, and a real approval
loop; in exchange, the platform is no longer swappable by changing a Trigger
image. Adding DingTalk or Slack means another plugin rather than another Trigger.
Worked example: `config/samples/coding/lark-coding-agent.yaml`.

---

## 15. FAQ

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
`kubectl port-forward` + `--tool-endpoint name=url`.

**`unknown field` on apply after a CRD change?** The cluster CRD is stale. Re-run
`make manifests` then `kubectl apply -k config/crd` (or `config/local`).

**Can the Registry leak secrets?** No. It only ships Secret name/key coordinates;
the value is read by the runtime via its own RBAC.

---

## 16. Repo layout

```
api/v1alpha1/           # 14 CRD types + shared types + structural validation (validation.go)
internal/controller/    # one reconciler per Kind; refutil.go = shared ref-resolution/watch helpers
internal/webhook/       # validating admission webhooks (Agent/Workflow/Tool)
cmd/main.go             # Operator (manager)
cmd/registry/           # Registry (data-plane config endpoint: /config, /watch; wire types from the SDK)
cmd/agent-runtime/      # reference runtime built on the Go SDK (one-shot + --chat + --serve + --watch)
cmd/example-mcp/        # minimal MCP server (test fixture)
config/crd|rbac|manager # kustomize bases
config/default          # full deploy (webhook/cert-manager)
config/local            # local overlay (no webhook)
config/registry         # in-cluster Registry (Deployment+Service+RBAC)
config/samples          # coherent sample resources
```

> `cmd/agent-runtime`, `cmd/example-mcp` are **verification fixtures**, not part
> of the control plane — they stand in for real Agent frameworks and MCP tool
> servers to prove the platform drives them end to end.
