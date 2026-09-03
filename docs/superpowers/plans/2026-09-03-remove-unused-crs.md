# Remove Workflow / Memory / KnowledgeBase CRs — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Delete the Workflow, Memory, and KnowledgeBase CR kinds end-to-end per `docs/superpowers/specs/2026-09-03-remove-unused-crs-design.md` — types, controllers, webhook, Registry payload assembly, RBAC, samples, docs.

**Architecture:** One atomic Go commit (types cannot vanish half-way and leave the tree compiling), then manifests/RBAC/samples, then docs. Payload wire types live in the external SDK (`sdk.AgentConfig`), so the Registry stops *populating* `Workflow`/`Memories`/`KnowledgeBases` and the SDK structs remain untouched until a separate SDK release — deleting fields there is not part of this plan.

**Tech Stack:** Go, controller-gen (`make manifests` / `make generate`), kustomize, envtest.

**Execute after:** `2026-09-03-remove-legacy-runtimes.md` (its doc edits already remove the biggest prose consumers of these CRs).

---

### Task 1: Atomic Go removal (types → controllers → webhook → registry)

Everything in this task is ONE commit — the tree must not compile half-way through.

**Files:**
- Delete: `api/v1alpha1/workflow_types.go`, `api/v1alpha1/memory_types.go`, `api/v1alpha1/knowledgebase_types.go`
- Delete: `internal/controller/workflow_controller.go`, `internal/controller/workflow_controller_test.go`, `internal/controller/memory_controller.go`, `internal/controller/memory_controller_test.go`, `internal/controller/knowledgebase_controller.go`, `internal/controller/knowledgebase_controller_test.go`
- Delete: `internal/webhook/v1alpha1/workflow_webhook.go`, `internal/webhook/v1alpha1/workflow_webhook_test.go`
- Modify: `api/v1alpha1/agent_types.go`, `api/v1alpha1/agentclass_types.go`, `api/v1alpha1/validation.go`, `api/v1alpha1/zz_generated.deepcopy.go` (regenerated)
- Modify: `cmd/main.go`, `cmd/registry/main.go`, `internal/controller/agent_controller.go`, `internal/controller/agent_controller_test.go`, `internal/controller/fieldindex.go`, `internal/controller/fieldindex_test.go`, `internal/controller/fieldindex_envtest_test.go`, `internal/controller/constants.go`, `internal/controller/refutil.go`, `internal/controller/resolution_test.go`

- [ ] **Step 1: Delete the type, controller, and webhook files**

```bash
git rm api/v1alpha1/workflow_types.go api/v1alpha1/memory_types.go api/v1alpha1/knowledgebase_types.go
git rm internal/controller/workflow_controller.go internal/controller/workflow_controller_test.go
git rm internal/controller/memory_controller.go internal/controller/memory_controller_test.go
git rm internal/controller/knowledgebase_controller.go internal/controller/knowledgebase_controller_test.go
git rm internal/webhook/v1alpha1/workflow_webhook.go internal/webhook/v1alpha1/workflow_webhook_test.go
```

- [ ] **Step 2: Remove the Agent spec fields** (`api/v1alpha1/agent_types.go`)

Delete these blocks (lines ~45-49, ~70-73, ~88-91):

```go
	// workflowRef references the Workflow describing the agent's execution
	WorkflowRef *LocalReference `json:"workflowRef,omitempty"`
```

```go
	// memoryRefs lists Memory backends available to the agent.
	MemoryRefs []LocalReference `json:"memoryRefs,omitempty"`
```

```go
	// knowledgeBaseRefs lists KnowledgeBases the agent may retrieve from (RAG).
	KnowledgeBaseRefs []LocalReference `json:"knowledgeBaseRefs,omitempty"`
```

And in `ApplyClassDefaults` (lines ~220-221):

```go
	if spec.WorkflowRef == nil && class.Spec.DefaultWorkflowRef != nil {
		spec.WorkflowRef = class.Spec.DefaultWorkflowRef
	}
```

- [ ] **Step 3: Remove the AgentClass default** (`api/v1alpha1/agentclass_types.go`, lines ~35-37)

```go
	// defaultWorkflowRef is applied when an Agent omits a workflowRef.
	DefaultWorkflowRef *LocalReference `json:"defaultWorkflowRef,omitempty"`
```

- [ ] **Step 4: Strip validation** (`api/v1alpha1/validation.go`)

- Delete `func (s *WorkflowSpec) Validate() error` and its helper usage (starts line ~37, comment above it included).
- Delete the duplicate-ref checks (lines ~102-104, ~111-113):

```go
	if dup := firstDuplicate(refNames(s.MemoryRefs)); dup != "" {
```

```go
	if dup := firstDuplicate(refNames(s.KnowledgeBaseRefs)); dup != "" {
```

(keep each block's `return`-shape consistent — remove the whole `if` block for each).

- [ ] **Step 5: Strip the agent controller** (`internal/controller/agent_controller.go`)

- Resolution targets (lines ~309-311, ~325-327, ~335-337): delete the three blocks that append `target{kind: "Workflow"/kindMemory/"KnowledgeBase", ...}` and the `res.declared.*` lines they carry; delete the `for _, ref := range eff.MemoryRefs` / `KnowledgeBaseRefs` loops entirely.
- Watches (lines ~489-490, ~505-509): delete the three `Watches(&corev1alpha1.Workflow{}, ...)`, `Watches(&corev1alpha1.Memory{}, ...)`, `Watches(&corev1alpha1.KnowledgeBase{}, ...)` entries.
- Sweep for stragglers: `grep -n "Workflow\|Memory\|KnowledgeBase" internal/controller/agent_controller.go` — expected remaining hits: none (comments referencing them go too).

- [ ] **Step 6: Strip fieldindex / constants / refutil** (`internal/controller/`)

`fieldindex.go`: delete constants `idxAgentWorkflow`, `idxClassWorkflow`, `idxMemoryConnection`, `idxKBMemory`, `idxKBCredential`, `idxKBModel` (and any other `idx*` whose registration references `Memory{}`/`KnowledgeBase{}`/`DefaultWorkflowRef`), plus their registration entries (lines ~134-136, ~150, ~159, ~180-182, ~212-223).

`constants.go`: delete `kindMemory = "Memory"` (line ~24) and any sibling `kindWorkflow`/`kindKnowledgeBase`.

`refutil.go`: update the comment (lines ~39-40) to drop "Memory" and "KnowledgeBase" from the list of reference-resolving controllers.

- [ ] **Step 7: Strip registrations** (`cmd/main.go`)

Delete the three controller setups (lines ~231-236, ~252-257, ~259-264) and the workflow webhook setup (lines ~303-305):

```go
	if err := (&controller.WorkflowReconciler{
```

```go
	if err := (&controller.MemoryReconciler{
```

```go
	if err := (&controller.KnowledgeBaseReconciler{
```

```go
		if err := webhookv1alpha1.SetupWorkflowWebhookWithManager(mgr); err != nil {
```

(each with its error-handling `if` block).

- [ ] **Step 8: Strip Registry payload assembly** (`cmd/registry/main.go`)

**CAREFUL: this file carries uncommitted WIP (the `/v1/models` handler). Edit only the blocks below; do not touch or revert anything near `handleModels`.**

- Workflow resolution (lines ~361-373): delete the block from `// Resolve the Workflow step graph the same way.` through `out.Workflow = wv` and its error branch.
- Memory loop (line ~430 `for _, ref := range eff.MemoryRefs`) through `out.Memories = append(...)` (~436): delete whole loop including the `mv` view struct usage.
- KnowledgeBase loop (line ~438): delete whole loop including `out.KnowledgeBases` population.
- Delete now-unused local view structs/helpers and their credential-resolution branches (the earlier-grep hits at lines ~200, ~387, ~585, ~609 assigning `SecretName` into memory/kb views — identify by `grep -n "workflowView\|memoryView\|kbView\|Workflow =\|Memories =\|KnowledgeBases =" cmd/registry/main.go`).
- `out.Workflow` / `out.Memories` / `out.KnowledgeBases` SDK struct fields stay declared in the SDK — this repo simply never sets them anymore.

- [ ] **Step 9: Fix the tests that referenced the kinds**

`grep -rn "Workflow\|Memory{\|KnowledgeBase" internal/controller/*_test.go` and update: delete fixtures/cases for the three removed kinds (e.g. `resolution_test.go`'s memory resolution case around lines 86-112). `agent_controller_test.go`, `fieldindex_test.go`, `fieldindex_envtest_test.go`: remove the assertions tied to the deleted indexes/kinds.

- [ ] **Step 10: Regenerate deepcopy and manifests**

```bash
make generate && make manifests
```

Expected: `zz_generated.deepcopy.go` loses the three kinds' methods; `config/crd/bases/` loses the three CRD YAMLs; `git status` shows those as deletions.

- [ ] **Step 11: Build and test**

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: PASS. If a test still constructs `Workflow{}`/`Memory{}`/`KnowledgeBase{}`, the compiler names it — remove that fixture.

- [ ] **Step 12: Commit**

```bash
git add -A
git commit -m "feat(api)!: remove Workflow, Memory, and KnowledgeBase CRs

No runtime consumes them (pi direction); declaration without a consumer
is decoration. Payload stops populating the sections; SDK wire types
follow in a separate SDK release."
```

---

### Task 2: RBAC and samples

**Files:**
- Modify: `config/rbac/role.yaml` (regenerated by `make manifests` — verify), `config/samples/travel/travel-agent.yaml`, `config/samples/travel/amap.yaml`

- [ ] **Step 1: Verify RBAC dropped the kinds**

Run: `grep -n "workflows\|memories\|knowledgebases" config/rbac/role.yaml`
Expected: no matches (controller-gen regenerates from the controllers deleted in Task 1; if matches remain, `make manifests` was not run or a `kubebuilder:rbac` marker survives — find it with `grep -rn "kubebuilder:rbac" internal/ | grep -iE "workflow|memory|knowledgebase"` and delete the marker, then re-run `make manifests`).

- [ ] **Step 2: Strip the travel-agent.yaml Workflow object and ref**

In `config/samples/travel/travel-agent.yaml`: delete the whole `kind: Workflow` document (starts line ~32, `name: travel-planning`, through the `---` that ends it) and the Agent's reference (lines ~121-122):

```yaml
  workflowRef:
    name: travel-planning
```

- [ ] **Step 3: Strip the amap.yaml ref**

In `config/samples/travel/amap.yaml`, delete (lines ~98-99):

```yaml
  workflowRef:
    name: travel-planning
```

- [ ] **Step 4: Commit**

```bash
git add config/rbac config/samples/travel
git commit -m "feat(samples): drop Workflow objects and refs from travel samples"
```

---

### Task 3: Docs

**Files:**
- Modify: `docs/runtime-protocol.md`, `README.md`, `docs/usage.md`

- [ ] **Step 1: runtime-protocol.md — delete the payload sections**

- §3: delete the `"workflow": { ... }` block (~lines 79-90), the `"memories": [ ... ]` block (~119-129), and the `"knowledgeBases": [ ... ]` block (~130-140), plus their bullet entries in the Conventions list ("`workflow` is the resolved step graph...", "`memories[]` carries only backend...", "`knowledgeBases[]` carries the corpus...").
- §8 Versioning: append one line to the version notes:

```text
- **v1.1 (2026-09-03):** removed `workflow`, `memories`, `knowledgeBases` from
  the payload along with their CRs; clients must treat absent sections as
  "not applicable". Breaking, published while no runtime existed.
```

- [ ] **Step 2: README.md — resource table**

Delete the three rows from the resource-model table:

```text
| **Workflow** | Engine-neutral execution shape (planner/tool/reflect/finish). |
```

```text
| **Memory** | A memory/storage backend (redis/postgres/vector/graph/s3). |
```

and the `**KnowledgeBase**` row (grep `grep -n "KnowledgeBase" README.md` — same shape).

- [ ] **Step 3: usage.md — residual mentions**

Run: `grep -n "Workflow\|Memory\|KnowledgeBase" docs/usage.md` and remove the remaining sections/bullets that describe the three kinds (the resource table in §2 and any §-body mention; §14's coding-agent content does not reference them). Section numbers stay stable — delete content only.

- [ ] **Step 4: Commit**

```bash
git add docs/ README.md
git commit -m "docs: drop Workflow/Memory/KnowledgeBase from the resource model and payload"
```

---

### Task 4: Final verification sweep

- [ ] **Step 1: Reference sweep**

```bash
grep -rn "WorkflowRef\|MemoryRefs\|KnowledgeBaseRefs\|kind: Workflow\|kind: Memory\|kind: KnowledgeBase" \
  --include="*.go" --include="*.yaml" --include="*.md" . | grep -v "^./docs/superpowers/"
```

Expected: no matches (historical specs under `docs/superpowers/` excluded).

- [ ] **Step 2: Full gate**

```bash
make generate && make manifests && git diff --exit-code
go build ./... && go vet ./... && go test ./...
```

Expected: regen idempotent (`git diff --exit-code` clean), build/vet/test PASS.

- [ ] **Step 3: Samples still apply (kind cluster, optional but recommended)**

```bash
kubectl apply -f config/samples/travel/travel-agent.yaml
kubectl apply -f config/samples/travel/amap.yaml
kubectl apply -f config/samples/travel/travel-assistant.yaml
```

Expected: accepted (unknown-field or NotFound errors mean a straggler reference survived).
