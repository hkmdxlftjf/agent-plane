# Design: 删除 Workflow / Memory / KnowledgeBase CR（pi 方向的 API 收缩）

Date: 2026-09-03

## 目标

删除 pi runtime 无法消费、且当前 repo 无任何实现的三个 CR 及全链路。
判断标准沿用 repo 自己的原则：没有 runtime 消费的能力就是装饰。

- **Workflow**：步骤图（planner/tool/reflect/finish），唯一解释器在已删
  的 SDK 示例里；pi 是 ReAct 循环，不是图引擎。
- **Memory**：只有 redis 在已删的参考 runtime 里实现过，其余 backend 本就
  "unsupported"；pi 无内建记忆。
- **KnowledgeBase**：只有 http-source 检索在已删 runtime 里实现过；pi
  无 RAG。

Trigger 保留（适配器契约 runtime 无关）。将来 pi 侧真需要记忆/RAG/流程
时，以"runtime 先能消费、CR 后声明"的顺序重新引入。

## 删除清单

**整文件删除**

- `api/v1alpha1/workflow_types.go`、`memory_types.go`、`knowledgebase_types.go`
- `internal/controller/workflow_controller.go`（+ `_test`）、
  `memory_controller.go`（+ `_test`）、`knowledgebase_controller.go`（+ `_test`）
- `internal/webhook/v1alpha1/workflow_webhook.go`（+ `_test`）
- `config/crd/bases/` 三个 CRD YAML（`make manifests` 重新生成）

**API 字段删除**

- `AgentSpec`：`workflowRef`、`memoryRefs`、`knowledgeBaseRefs`
- `AgentClassSpec`：`defaultWorkflowRef`（无 defaultMemoryRefs /
  defaultKnowledgeBaseRefs，仅此一个）
- `ApplyClassDefaults` 的 workflowRef 分支
- `api/v1alpha1/validation.go` 中 Workflow 结构校验（step 名唯一、next
  不悬空）及 Agent 校验里对这三个 ref 的一切检查
- `zz_generated.deepcopy.go` 重新生成（`make generate`）

**Controller / Registry**

- `agent_controller.go`：三个 kind 的引用解析、缺失项 Degraded 判定、
  configHash 折叠、enqueueReferrers watch
- `refutil.go`、`fieldindex.go`、`constants.go` 中三个 kind 的条目
- `cmd/main.go`：三个 controller 与 workflow webhook 的注册
- `cmd/registry/main.go`：payload 的 `workflow` / `memories` /
  `knowledgeBases` 三段组装（约 363-372、430-438 行区域）。
  **注意：该文件有在途的 `/v1/models` WIP（未提交），修改不得触碰**

**RBAC / 样例 / 文档**

- `config/rbac/`：三个 kind 的角色规则
- `config/samples/travel/travel-agent.yaml`：删除其中 `kind: Workflow`
  对象与 Agent 的 `workflowRef`；`amap.yaml` 的 `workflowRef` 同删
- `docs/runtime-protocol.md`：payload 三节删除；资源模型表述同步；
  记一笔 v1.1 破坏性变更（见下）
- `README.md` 资源表三行、`docs/usage.md` 相关段落
- 今日 secret-env spec 同步收缩（见连锁）

## 兼容性说明

- payload 删字段违反 v1 "append-only" 承诺。可接受的理由：所有已知
  runtime 已删（remove-legacy-runtimes）、pi 未落地，此刻是唯一无损窗口；
  在 runtime-protocol.md 的版本小节明记 v1.1 移除了这三个字段。
- SDK（agent-plane-sdk-go，独立仓库）wire 类型随后同步：删字段对"忽略
  未知字段"的客户端无害，属可延后动作。
- 集群中已存在的三类 CR 对象随 CRD 删除消失，无迁移路径（有意为之）。

## 连锁

- secret-env spec（2026-09-03-secret-env-injection-design.md）收缩：
  `memories[].env` / `knowledgeBases[].env` 相关内容删除，注入与保留名
  只剩 `AGENTPLANE_MODEL_API_KEY` 一个静态名（动态名推导随之消失，
  方案简化）。
- travel 样例删引用后仍可整体 apply（travel-assistant 不受影响）。

## 验证

1. `make manifests && make generate` 后 `git diff` 无残留。
2. `go build ./... && go vet ./... && go test ./...` 通过。
3. `grep -rn "WorkflowRef\|MemoryRefs\|KnowledgeBaseRefs\|kind: Workflow\|kind: Memory\|kind: KnowledgeBase"` 仅历史 spec 命中。
4. travel 样例 apply 无报错（本地 envtest 或 kind）。

## 测试

- envtest 全量通过（删掉的三个 controller 测试随之消失）；纯删除，
  无新增测试。
