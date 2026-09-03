# Remove Legacy Runtimes — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Delete `cmd/agent-runtime` and `cmd/node-agent-runtime` (code, images, CI, samples, docs) per `docs/superpowers/specs/2026-09-03-remove-legacy-runtimes-design.md`, preserving in-flight WIP on an archive branch.

**Architecture:** Pure removal. Archive the uncommitted node-agent-runtime WIP to a branch first, then `git rm` the runtime code/images/samples and rewrite the prose references (README, usage.md, runtime-protocol.md, policy.yaml comment) so nothing points at deleted paths.

**Tech Stack:** Go (operator), git, GitHub Actions matrix build.

---

### Task 1: Archive in-flight node-agent-runtime WIP

**Files:** none modified in the working branch (archive branch only)

- [ ] **Step 1: Create archive branch and commit the WIP**

```bash
git checkout -b archive/node-agent-runtime
git add cmd/node-agent-runtime config/samples/travel/demo/ox-alpha-model.yaml
git commit -m "archive: node-agent-runtime WIP (web UI rework) before removal"
```

- [ ] **Step 2: Return to the working branch**

```bash
git checkout feat/kind-local-deploy
```

- [ ] **Step 3: Verify the working tree is clean of runtime WIP**

Run: `git status --short`
Expected: only `M cmd/registry/main.go` remains (the `/v1/models` WIP — untouched, not part of this removal). No `cmd/node-agent-runtime` entries, no untracked `web/`, no `ox-alpha-model.yaml`.

---

### Task 2: Delete runtime code, Dockerfiles, demo samples, quickstart doc

**Files:**
- Delete: `cmd/agent-runtime/` (whole dir), `cmd/node-agent-runtime/` (whole dir)
- Delete: `Dockerfile.agent-runtime`, `Dockerfile.node-agent-runtime`
- Delete: `config/samples/travel/demo/` (whole dir)
- Delete: `docs/quickstart-custom-agent.md`

- [ ] **Step 1: Remove the files**

```bash
git rm -r cmd/agent-runtime cmd/node-agent-runtime
git rm Dockerfile.agent-runtime Dockerfile.node-agent-runtime
git rm -r config/samples/travel/demo
git rm docs/quickstart-custom-agent.md
```

- [ ] **Step 2: Verify the tree still builds**

Run: `go build ./... && go vet ./...`
Expected: PASS (cmd/* packages are mains, nothing imports them)

- [ ] **Step 3: Commit**

```bash
git commit -m "feat(runtime)!: remove cmd/agent-runtime and cmd/node-agent-runtime

pi is the only runtime direction; the opencode coding-agent image stays
until the pi runtime replaces it. WIP preserved on archive/node-agent-runtime."
```

---

### Task 3: CI workflow

**Files:**
- Modify: `.github/workflows/images.yml:5-7,34-35`

- [ ] **Step 1: Remove the agent-plane-runtime matrix entry**

In `.github/workflows/images.yml`, delete these two lines (34-35):

```yaml
          - image: agent-plane-runtime
            dockerfile: Dockerfile.agent-runtime
```

- [ ] **Step 2: Fix the header comment (lines 5-7)**

Replace:

```text
# Images land at ghcr.io/<owner>/{agent-plane,agent-plane-registry,
# agent-plane-runtime,agent-plane-example-mcp,agent-plane-coding-agent,
# agent-plane-lark-mcp}.
```

with:

```text
# Images land at ghcr.io/<owner>/{agent-plane,agent-plane-registry,
# agent-plane-example-mcp,agent-plane-coding-agent,agent-plane-lark-mcp}.
```

- [ ] **Step 3: Validate the YAML**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/images.yml'))"`
Expected: no output (valid YAML)

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/images.yml
git commit -m "ci: stop building the removed agent-plane-runtime image"
```

---

### Task 4: README.md

**Files:**
- Modify: `README.md:138-175,196-197`

- [ ] **Step 1: Delete the "Reference runtime" section (lines 138-175)**

Delete the whole block starting `## Reference runtime: a real agent driven by Agent Plane` and ending `...deploying your own agent.` — from the `## Reference runtime` heading through the paragraph that links `docs/quickstart-custom-agent.md` (inclusive). The next heading, `## Operator-managed runtime (`spec.runtime`)`, stays.

- [ ] **Step 2: Reword the hot-reload sentence (lines 196-197)**

Replace:

```text
Registry and hot-reloads on change. The reference image (`Dockerfile.agent-runtime`,
`cmd/agent-runtime --watch`) does exactly this. In-cluster Registry manifests are
```

with:

```text
Registry and hot-reloads on change. In-cluster Registry manifests are
```

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs(readme): drop the reference-runtime walkthrough"
```

---

### Task 5: docs/usage.md

**Files:**
- Modify: `docs/usage.md` — §7 body (~lines 224-240), §8 reference-image block (~374-379), §9 (~392-417), §10 ② (~423-426), §16 (~806, 815)

Section numbers are load-bearing (other docs reference "usage.md §14") — delete content, never renumber.

- [ ] **Step 1: Replace §7 body**

Replace the section body (from `` `cmd/agent-runtime` is a minimal but real runtime`` through the closing ``` ``` ``` of the `go run` block) with:

```markdown
Any runtime that implements `docs/runtime-protocol.md` runs here: it pulls
config from the Registry, resolves `http` tools via POST and `mcp` tools via
JSON-RPC `tools/call`, feeds results back until a final answer, and refuses
calls its Policy/ToolPolicy forbids. The reusable Go loop lives in the SDK
([`agent-plane-sdk-go`](https://github.com/hkmdxlftjf/agent-plane-sdk-go));
for a runnable agent today see §14 (coding agent) — the in-repo reference
runtime was removed in favor of the pi direction.
```

- [ ] **Step 2: Remove the §8 reference-image block**

Delete (from `**Reference runtime image.**` through the `docker build -f Dockerfile.agent-runtime ...` fenced block inclusive). Keep the `> The runtime pod needs RBAC...` note and the `> **Push vs pull:**` note that follow it.

- [ ] **Step 3: Replace §9 body**

Replace the §9 body (from `` `cmd/agent-runtime` is a minimal real agent`` through the `See **[quickstart-custom-agent.md...` line inclusive) with:

```markdown
The in-repo reference runtime was removed (pi is the only runtime direction;
see §14 for the coding-agent path that runs today). For the contract a new
runtime implements, see **[runtime-protocol.md](runtime-protocol.md)**.
```

- [ ] **Step 4: Fix §10's ② line**

Replace:

```text
# ② Real agent: real model + real tool call (see §9)
go run ./cmd/agent-runtime --namespace <ns> --name <agent> --prompt "..."
```

with:

```text
# ② Real agent: see §14 (coding agent) — the reference runtime was removed
```

- [ ] **Step 5: Fix §16 repo layout**

In the layout code block, delete this line:

```text
cmd/agent-runtime/      # reference runtime built on the Go SDK (one-shot + --chat + --serve + --watch)
```

Replace the fixtures note below it:

```text
> `cmd/agent-runtime`, `cmd/example-mcp` are **verification fixtures**, not part
> of the control plane — they stand in for real Agent frameworks and MCP tool
> servers to prove the platform drives them end to end.
```

with:

```text
> `cmd/example-mcp`, `cmd/script-mcp` are **verification fixtures**, not part
> of the control plane — they stand in for real MCP tool servers to prove the
> platform drives them end to end.
```

- [ ] **Step 6: Commit**

```bash
git add docs/usage.md
git commit -m "docs(usage): remove reference-runtime sections, keep numbering stable"
```

---

### Task 6: docs/runtime-protocol.md

**Files:**
- Modify: `docs/runtime-protocol.md:9-12,268-270`

- [ ] **Step 1: Drop the cmd/agent-runtime clause from the header**

Replace:

```text
Reference implementation: `cmd/registry/` (server) and the Go SDK
[`github.com/hkmdxlftjf/agent-plane-sdk-go`](https://github.com/hkmdxlftjf/agent-plane-sdk-go)
(client — wire types, `FetchConfig`/`Watch`, secret reads); `cmd/agent-runtime/`
is a complete runtime built on it.
```

with:

```text
Reference server: `cmd/registry/`. Client reference: the Go SDK
[`github.com/hkmdxlftjf/agent-plane-sdk-go`](https://github.com/hkmdxlftjf/agent-plane-sdk-go)
(wire types, `FetchConfig`/`Watch`, secret reads). This document is normative
on its own; there is no in-repo runtime.
```

- [ ] **Step 2: Remove the §6 implementation pointer**

Delete this line (~270):

```text
A working `--watch` implementation of this loop is in `cmd/agent-runtime/`.
```

(The pseudocode above it remains the normative loop.)

- [ ] **Step 3: Commit**

```bash
git add docs/runtime-protocol.md
git commit -m "docs(protocol): drop references to the removed runtime"
```

---

### Task 7: travel policy comment + final sweep

**Files:**
- Modify: `config/samples/travel/policy.yaml:27`

- [ ] **Step 1: Fix the policy.yaml comment**

Replace:

```text
#   go run ./cmd/agent-runtime --namespace <ns> --name travel-agent --chat
```

with:

```text
#   (any runtime implementing docs/runtime-protocol.md logs its effective
#    policy at startup)
```

- [ ] **Step 2: Full verification sweep**

```bash
go build ./... && go vet ./... && go test ./...
grep -rn "agent-runtime\|node-agent-runtime" --include="*" . \
  | grep -v "^./docs/superpowers/" | grep -v "^./.git/"
```

Expected: build/vet/test PASS; grep clean (historical specs under `docs/superpowers/` are the only allowed matches). If `docs/adapter-protocol.md:36` matches, it is the generic example URL `support-agent-runtime.default.svc` — allowed to stay.

- [ ] **Step 3: Commit**

```bash
git add config/samples/travel/policy.yaml
git commit -m "docs(samples): travel policy comment no longer invokes the removed runtime"
```
