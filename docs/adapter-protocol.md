# Agent Plane Inbound Adapter Contract (v1)

This spec defines how an **inbound adapter** — the process that brings messages
from Lark, DingTalk, Slack, or anything else into an Agent — is wired and what it
must do. Implement these three rules in any language and your adapter runs under
Agent Plane without a control-plane change.

It is the mirror of [runtime-protocol.md](runtime-protocol.md): that one covers
config flowing *out* to a runtime, this one covers events flowing *in*.

---

## 1. Why a contract instead of built-in integrations

Agent Plane implements no platform protocol. Each IM differs in how it delivers
events (Lark WebSocket subscription, DingTalk Stream, Slack Socket Mode), how it
authenticates, and how a reply is addressed — and none of that belongs in a
control plane.

So the control plane owns the part that *is* uniform: scheduling the adapter,
handing it credentials, and telling it where the Agent is. Everything
platform-specific stays in the adapter image.

**Adding a platform = a new adapter image + a `Trigger`.** No operator change, no
new CRD, no rebuild of anything in this repo.

---

## 2. What the Operator gives you

Declare a `Trigger` and the Operator materializes an adapter Deployment it owns,
with this environment injected:

| Variable | Always? | Meaning |
|---|---|---|
| `AGENTPLANE_AGENT_ENDPOINT` | yes | Base URL of the Agent's runtime, e.g. `http://support-agent-runtime.default.svc:8080`. **Do not construct this yourself.** |
| `AGENTPLANE_AGENT_NAME` | yes | The Agent's name, for logging. |
| `AGENTPLANE_AGENT_NAMESPACE` | yes | The Agent's namespace. |
| `AGENTPLANE_TRIGGER_NAME` | yes | This Trigger's name, for logging. |
| `AGENTPLANE_TRIGGER_CONFIG` | when `spec.config` is set | Your `spec.config`, serialized verbatim. Absent, not empty, when unset. |
| `AGENTPLANE_CREDENTIAL_PATH` | when `spec.credentialRef` is set | Directory holding the credential Secret's keys as files. Absent, not empty, when unset. |

These names are reserved: a `Trigger` that sets one in `spec.env` is rejected at
apply time rather than silently pointing the adapter at the wrong Agent.

Credentials arrive as a **read-only mount**, not as environment values, so they
stay out of `kubectl describe pod` and the process environment. Each key of the
Secret is a file: a Secret with `app-id` and `app-secret` yields
`$AGENTPLANE_CREDENTIAL_PATH/app-id` and `$AGENTPLANE_CREDENTIAL_PATH/app-secret`.

---

## 3. What your adapter must do

**Rule 1 — deliver each message to the Agent.**

```
POST $AGENTPLANE_AGENT_ENDPOINT/api/chat
Content-Type: application/json

{"sessionId": "<conversation id>", "message": "<user text>"}
```

Response:

```json
{"answer": "…"}          // success
{"error": "…"}           // the runtime failed; still HTTP 200
```

Check for `error` — it arrives with HTTP 200, not a 5xx.

**Rule 2 — use the platform's conversation id as `sessionId`.**

This is the one field with real consequences. The runtime keys conversation
memory on it, so a stable id per chat gives you multi-turn context for free, and
one id shared across chats leaks history between users. Use Lark's `chat_id`,
DingTalk's `conversationId`, Slack's channel+thread. Never a constant.

**Rule 3 — post the answer back yourself.**

The Agent does not know it is talking to Lark. Your adapter calls the platform's
reply API with whatever addressing that platform needs (Lark wants the
`message_id` to reply to; DingTalk has a time-limited `sessionWebhook`).

That is the whole contract. Health probing is optional but recommended: serve
`/healthz` and set a readiness probe in `spec.env`-adjacent pod config if you
need the Trigger's `availableReplicas` to mean something stricter.

---

## 4. Worked example

```yaml
apiVersion: v1
kind: Secret
metadata: {name: lark-app}
stringData:
  app-id: cli_xxx
  app-secret: yyy
---
apiVersion: core.hkmdxlftjf.io/v1alpha1
kind: Credential
metadata: {name: lark-app}
spec:
  secretRef: {name: lark-app, key: app-secret}   # the whole Secret is mounted
---
apiVersion: core.hkmdxlftjf.io/v1alpha1
kind: Agent
metadata: {name: support-agent}
spec:
  modelRef: {name: llm-model}
  runtime:
    image: ghcr.io/hkmdxlftjf/agent-plane-runtime:latest
    port: 8080                 # REQUIRED: this is what creates the Service
    env:
      - {name: AGENTPLANE_SERVE, value: "1"}    # serve mode exposes /api/chat
---
apiVersion: core.hkmdxlftjf.io/v1alpha1
kind: Trigger
metadata: {name: lark-support}
spec:
  agentRef: {name: support-agent}
  image: myorg/lark-adapter:v1
  credentialRef: {name: lark-app}
  config:
    events: ["im.message.receive_v1"]
```

```sh
kubectl get trigger lark-support
# NAME           AGENT           PHASE     AVAILABLE   AGE
# lark-support   support-agent   Running   1           30s
```

**The Agent must set `spec.runtime.port`.** Without it no Service exists and
there is nothing for the adapter to POST to; the Trigger goes `Degraded` saying
so, rather than injecting an address nothing listens on.

---

## 5. A minimal adapter, in outline

```go
endpoint := os.Getenv("AGENTPLANE_AGENT_ENDPOINT")
appSecret, _ := os.ReadFile(filepath.Join(os.Getenv("AGENTPLANE_CREDENTIAL_PATH"), "app-secret"))

for ev := range platformEvents {           // your long-lived connection
    body, _ := json.Marshal(map[string]string{
        "sessionId": ev.ChatID,            // Rule 2
        "message":   ev.Text,
    })
    resp, err := http.Post(endpoint+"/api/chat", "application/json", bytes.NewReader(body))
    if err != nil { continue }             // log, keep the connection alive

    var out struct{ Answer, Error string }
    _ = json.NewDecoder(resp.Body).Decode(&out)
    if out.Error != "" { continue }

    platformReply(ev, out.Answer)          // Rule 3
}
```

---

## 6. Operational notes

**Replicas.** `spec.replicas` is capped at 1. A streaming adapter holds one
connection, and most platforms deliver each event to *every* connection — two
replicas usually means answering each message twice. Shard or elect a leader
inside the adapter if you need more.

**Reconnection is yours.** The Operator restarts a crashed pod, but a dropped
socket that leaves the process running looks healthy to Kubernetes. Reconnect
with backoff, and exit non-zero when you cannot recover so the pod restarts.

**Duplicate delivery is yours.** Platforms retry. If double-answering matters,
dedupe on the platform's message id.

**What `Running` means.** The adapter pod is up and scheduled. It does *not*
mean the adapter authenticated or connected — only the adapter knows that, and
the contract deliberately does not require it to report back. Treat `Running` as
"the process exists", and put connection health in your own logs and metrics.

**Changing the Agent's port** re-reconciles the Trigger and rolls the adapter
with the new endpoint; the Trigger watches its Agent.

---

## 7. Outbound is a different mechanism

This contract is for events coming *in*. For the Agent to *call* something —
sending a proactive message, looking up a record — declare a `Tool`, usually
`type: mcp` pointing at that platform's MCP server. See
[usage.md](usage.md) §4. The two compose: an adapter brings the question in, and
Tools let the Agent act while answering it.
