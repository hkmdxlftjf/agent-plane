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

A runtime **may** additionally return `confirmation` alongside `answer`, when
the model wants to pause a destructive or uncertain action and hand the
decision to the user rather than guess:

```json
{
  "answer": "我准备把当前分支推送到 origin/main,请确认。",
  "confirmation": {
    "summary": "push the current branch to origin/main",
    "options": [
      {"label": "同意", "value": "approve"},
      {"label": "拒绝", "value": "reject"}
    ]
  }
}
```

This is additive, not a replacement for `answer` — an adapter that doesn't
look at `confirmation` still shows the user something reasonable, because
`answer` already describes the pending action in prose. An adapter that does
render it (as an interactive card with one button per option, say) must feed the
user's choice back through the **same** `sessionId` as an ordinary
`POST /api/chat` message — a card click is not a new wire call, just a different
way of producing the next `message`.

Two things this field cannot do, and they are why the coding-agent runtime no
longer uses it (§7):

- **A prompt cannot enforce it.** For the runtime to pause, the model has to
  choose to call the tool that pauses. Nothing stops it from doing the risky
  thing first and asking afterwards.
- **The pause is a new turn, not a suspended one.** The runtime answers, the
  adapter asks, the user replies, and the runtime starts over from a fresh
  message — so whatever exploration led to the question is either redone or
  lost. A runtime whose agent loop can genuinely block mid-turn (opencode's
  permission system does) gets a stronger guarantee by keeping the approval
  inside its own process.

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

## 5. Decisions any adapter faces

This repository no longer ships a Go adapter — the Lark integration moved into
the coding-agent runtime as an opencode plugin (§7), and the platform-specific
logic went with it. The decisions below were made against the real platform and
apply to any adapter, in any language; they are the expensive part to rediscover.

**Long connection over webhook.** Dial out and hold the socket, so nothing has to
reach the pod from the internet — no ingress, no TLS certificate, no public URL to
register. A cluster with only egress can run it.

**A failed turn is answered, not retried.** Do not return an error to the
platform SDK when the *agent* failed: most SDKs treat that as undelivered and the
platform redelivers, so a broken agent is asked the same question forever. Put
the error in the chat instead.

**Non-text messages get a reply, not silence.** An image or file forwarded raw
reaches the model as the JSON of its payload. Saying "I can only read text" is a
better failure than a confused answer.

**Credentials are read from files and trimmed.** They arrive as a mount, and a
trailing newline — what `--from-file` and most editors produce — makes the
handshake fail with an error that never mentions whitespace.

**Deduplicate on the platform's event id.** Platforms retry. A redelivered
message costs an extra model call; a redelivered button click can register a
second decision, or race itself updating the same card.

**Answer an interaction callback immediately, then follow up.** Button and card
callbacks have short deadlines that a real agent turn will miss. Acknowledge
receipt synchronously, do the work in the background, and send the answer as a
new message when it is ready.

Configuration read from `spec.config` (both optional, and what the Go adapter
used to accept):

| Key | Default | Meaning |
|---|---|---|
| `replyInThread` | `true` | Attach the answer to the message that prompted it. |
| `timeoutSeconds` | `300` | How long to wait for `/api/chat`. An agent that reads code needs more than an HTTP default. |

---

## 6. A minimal adapter, in outline

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

## 7. Operational notes

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

## 8. Outbound is a different mechanism

This contract is for events coming *in*. For the Agent to *call* something —
sending a proactive message, looking up a record — declare a `Tool`, usually
`type: mcp` pointing at that platform's MCP server. See
[usage.md](usage.md) §4. The two compose: an adapter brings the question in, and
Tools let the Agent act while answering it.

---

## 9. When this contract does not apply

This contract assumes the platform connection and the agent are **separate
processes**: the adapter POSTs, the runtime answers. That separation is what makes
the platform swappable — a new IM is a new image and a new `Trigger`, with no
control-plane change.

It costs something, and for one shape the cost is too high. A **coding agent
whose approvals must be enforced** needs the thing that blocks a tool call and the
thing that shows the user a button to be the same process. Over this contract they
cannot be: `/api/chat` is request/response, so a runtime that wants approval has
to end its turn, return a `confirmation` (§3), and be asked again — and the model
must have volunteered to pause in the first place. A model that decides not to
call the pause tool simply does not pause.

So `Dockerfile.coding-agent` does not implement this contract. It runs opencode
with a plugin that holds the Lark connection in-process, and lets opencode's own
permission system block the turn. The approval is enforced by the tool layer
rather than requested in a prompt, and answering it *resumes* the paused turn
instead of starting a new one — the work leading up to the question is not redone.

The tradeoff is explicit, and it is the mirror of §1: one moving part instead of
two, and a real approval loop, in exchange for a platform that is no longer
swappable by changing a `Trigger` image. Adding DingTalk there means another
plugin, not another Trigger.

**Which to use.** If the Agent answers questions, use this contract — it is
simpler, and the adapter is reusable across runtimes. If the Agent writes to a
repository and you need approvals that hold even when the model would rather not
ask, put the platform inside the runtime. See [usage.md](usage.md) §14.
