# CogNet Runtime Configuration & Change-Notification Protocol (v1)

This spec defines **how the control plane (via the Registry) notifies a data-plane
runtime of changes to an Agent and its dependencies**, and how the runtime should
consume them. Any Agent runtime (LangGraph / Agents SDK / CrewAI / custom) that
implements this spec can plug in — without reading the Kubernetes API or knowing
CogNet internals.

Reference implementation: `cmd/registry/` (server), `cmd/agent-runtime/` +
`internal/agentloop/` (client).

---

## 1. Design principles

1. **Full snapshots, not deltas.** Every notification is the complete config that
   should currently apply. → Simple, idempotent runtimes; dropped events still
   converge.
2. **One change token, `configHash`.** The control plane folds the Agent and all
   its transitive dependencies into a single hash; the runtime only compares the
   hash to decide whether to reload.
3. **Config/secret separation.** The payload carries Secret **coordinates**
   (name/key), never the value; the runtime reads the Secret via its own RBAC.
4. **The control plane never triggers inference.** The protocol answers "what
   config", not "when to run"; triggering is the runtime's concern.

---

## 2. Transport & endpoints

- HTTP/1.1; JSON is UTF-8. Base like `http://<registry-host>:9090`.
- Protocol version is in the path prefix `/v1`.

| Method | Path | Description |
|---|---|---|
| GET | `/v1/agents/{namespace}/{name}/config` | One-shot snapshot. |
| GET | `/v1/agents/{namespace}/{name}/watch` | Change stream (SSE). |
| GET | `/healthz` | Liveness, returns `ok`. |

### `/config`
- `200 OK`, `application/json`, body is an `AgentConfig`.
- `404` if the Agent doesn't exist; `400` on a malformed path.

### `/watch` (SSE)
- `200 OK`, `Content-Type: text/event-stream`.
- Event rules:
  1. Sends a snapshot **immediately** on connect (if the Agent exists).
  2. Sends a full snapshot **whenever the Agent changes**.
  3. Sends `: keepalive` comment lines every ~25s (SSE comment — ignore).
- Frame format:
  ```
  data: {"namespace":"default","name":"support-agent","configHash":"…", …}\n\n
  ```
- No resume cursor; on reconnect the client gets a fresh initial snapshot (§6).

---

## 3. `AgentConfig` payload

```jsonc
{
  "namespace": "default",
  "name": "support-agent",
  "configHash": "bd6a92…",          // sha256 hex; "" when Degraded
  "phase": "Ready",                  // Pending | Ready | Degraded
  "spec": { /* Agent.spec verbatim, incl. *Ref fields */ },

  "model": {                         // present when the Agent is Ready
    "provider":  "custom",
    "modelName": "claude-haiku-4-5-20251001",
    "endpoint":  "http://gateway/v1",
    "secretName":"llm-secret",       // ← coordinates only
    "secretKey": "api-key"           //    the value is NOT in the payload
  },

  "tools": [                         // fully-resolved, directly invocable
    {
      "name":        "order-lookup",
      "type":        "mcp",          // http | mcp | …
      "description": "Look up a customer order…",
      "endpoint":    "http://orders-mcp.default.svc:8080",   // mcp: from MCPServer.status.endpoint
      "mcpToolName": "get_order_status",
      "inputSchema": { "type":"object", "properties":{ "orderId":{"type":"string"} }, "required":["orderId"] }
    }
  ],

  "skills": [ "refund-handling" ]    // markdown pack names (body fetched via K8s)
}
```

Conventions:
- When not Ready, `model` may be absent and `configHash` is `""`.
- `tools[].endpoint`: for `http` it's the tool's own URL; for `mcp` it's the
  backing MCPServer's in-cluster endpoint.
- Semantics are append-only across versions; clients must ignore unknown fields
  (forward compatible).

---

## 4. `configHash` semantics (the change token)

- Computed over the Agent's **resolved reference set** — each ref is
  `{kind, name, resourceVersion}`, stable order, then sha256. See
  `configHash()` in `internal/controller/agent_controller.go`.
- Changes **iff** a reference is added/removed, or a referenced object
  (Model/Tool/Skill/…) changes (its resourceVersion changes).
- Fields **not** in the hash (e.g. the Agent's `description`) may change without
  changing the hash — i.e. changes that don't affect runtime config don't trigger
  a reload.
- Degraded ⇒ `configHash = ""`.

So the runtime's decision is trivial: `newHash != currentHash` ⇒ reload; else ignore.

---

## 5. Delivery guarantees & reconnection

- **At-least-once, intermediate states may be dropped.** The server broadcast is
  non-blocking (a slow consumer drops intermediate snapshots), but because each
  frame is the full latest state and last-writer-wins, the runtime always
  converges to the latest.
- **No resume cursor.** On reconnect the server sends an authoritative initial
  snapshot → self-healing; there is no "missed events during reconnect" problem.
- The client **SHOULD** reconnect on disconnect (with backoff); **MAY** fall back
  to polling `/config`.

---

## 6. Runtime obligations (MUST / SHOULD)

- **MUST** treat each event as a full snapshot; implementations must be idempotent.
- **MUST** read the model key from the referenced Secret via its own Kubernetes
  RBAC using `secretName`/`secretKey`; **must not** expect the Registry to send
  the value.
- **SHOULD** compare `configHash` and skip no-op events (and keepalives).
- **SHOULD** keep the last Ready config when `phase != Ready` (or refuse per
  policy); don't overwrite with an empty config.
- **SHOULD** reconnect (exponential backoff); **MAY** poll `/config` as a fallback.
- **Hot swap MUST be atomic**: rebuild the tool table / model client, then switch
  as a whole so in-flight requests are unaffected (RWMutex / pointer swap).

Minimal consumer loop (pseudocode):
```text
open SSE /watch
on snapshot s:
    if s.configHash == current.configHash: continue        # includes keepalives
    tools  := build_tools(s.tools)                          # rebuild from new config
    key    := k8s_get_secret(s.model.secretName)[s.model.secretKey]
    atomically swap {current = s, tools, key}
on disconnect: backoff, reconnect                            # reconnect ⇒ fresh full snapshot
```

A working `--watch` implementation of this loop is in `cmd/agent-runtime/`.

---

## 7. Errors & edge cases

| Situation | Server | Runtime should |
|---|---|---|
| Agent missing | `/config` → 404; `/watch` holds, no snapshot yet | retry; keep last config |
| Agent Degraded | snapshot `phase=Degraded`, `configHash=""` | keep last Ready config |
| Dependency created later | Agent auto-converges → new snapshot pushed | hot-reload on receipt |
| MCPServer not ready | `tools[].endpoint` may be empty | that tool is unavailable; don't crash |
| Connection dropped | — | reconnect; initial snapshot self-heals |

---

## 8. Versioning & evolution

- Current version `v1` (path prefix). Breaking changes bump to `/v2`, served in
  parallel during transition.
- Forward-compat: additive fields only; clients ignore unknown fields.
- Planned (not part of the v1 contract): `ETag`/`If-None-Match` for polling
  short-circuit; SSE `id:` + `Last-Event-ID` resume cursor; gRPC/server-streaming
  transport; event-level `kind` (`snapshot`/`deleted`).

---

## 9. Relationship to the control plane

```
Tool/Skill/Model change
      │  (AgentReconciler uses .Watches + enqueueReferrers to find Agents referencing it)
      ▼
Agent reconciles → recomputes configHash → writes Agent.status
      │  (Registry uses an informer watching only Agents)
      ▼
Registry.onAgentChange → assembles AgentConfig → hub.broadcast
      │  (this protocol: SSE /watch full snapshot)
      ▼
Runtime compares configHash → atomic hot-swap → keeps serving
```

That is: **"what changed" is folded by the control plane into "one new config + one
new hash"; the protocol carries no field-level diff** — the core trade-off of this
spec (simple, self-healing, idempotent). A runtime that wants field-level diffs
computes them from two snapshots itself.
