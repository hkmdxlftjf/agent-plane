# Design: Secret 经环境变量注入（runtime protocol v1.1）

Date: 2026-09-03

## 背景

v1 协议（docs/runtime-protocol.md §1.3/§6）规定 payload 只携带 Secret 坐标
（secretName/secretKey），runtime **必须**自备 Kubernetes RBAC 读值。后果：

- 文档自己记录的 footgun：忘记绑 `get secrets` Role → pod 正常启动、每轮
  请求失败（§1.3）。
- 每个 runtime 都要带一个 kube client：Go SDK 有 `secrets.NewReader`；
  node-agent-runtime 手写了 57 行 in-cluster client
  （cmd/node-agent-runtime/src/secrets.js，后随 remove-legacy-runtimes
  设计删除）只为把坐标换成值；pi 迁移评估
  中这是明确成本项——且 kube API 凭据住在跑模型 shell 命令的同一进程里
  （agent_controller.go:552-562 的注释已为此把 model secret 挂成了文件）。

"值不走 RBAC"在 repo 里已有三条先例（model secret 文件挂载、Credential CR
文件挂载、git credential 文件挂载）。本设计把这条线走完：**env 注入成为
协议的一等路径，RBAC 读取降级为回退**。

## 目标

- 所有 Agent runtime pod：model 的 Secret 以 `valueFrom: secretKeyRef`
  注入 env，变量名由 Registry payload 携带。
  （2026-09-03 更新：Memory/KnowledgeBase CR 已随 remove-unused-crs
  设计删除，原方案中 `memories[].env` / `knowledgeBases[].env` 两节
  随之取消；将来重引入这些 CR 时再扩展。）
- 协议义务反转：Operator 管辖下的 runtime 不再需要 kube client 和 RBAC。
- 兼容：AgentConfig 只增字段；旧 runtime 仍可用坐标 + RBAC 读。

## 非目标

- Credential CR 文件挂载（`/var/run/agentplane/credentials`）改 env——
  多 key 集合，文件形态保持。
- 移除 model secret 文件挂载（`/var/run/agentplane/model`）——保留并标注
  legacy，待 pi runtime 落地后另议。
- Secret 轮换的热更新（见"轮换语义"，如实写死为需要 rollout）。
- Trigger/adapter 契约（`AGENTPLANE_CREDENTIAL_PATH` 文件挂载）不动。

## 方案

### 原则

**exec 时就要用的值走 env；拉到 payload 才知道的走 payload。** payload
永不携带 Secret 值（不变）；env 变量*名*属于 payload（runtime 不猜）。

### 命名规则（单一实现，双侧使用）

| 坐标 | 变量名 |
|---|---|
| `model` | `AGENTPLANE_MODEL_API_KEY` |

单一静态名，无推导。规则实现为一个共享常量，Registry（写进 payload）
与 Operator（注入 env）两侧共用；SDK 不需要规则——读 `payload.model.env`
即可。

### AgentConfig 增字段（additive，符合 v1 前向兼容）

`model.env`：string，注入的变量名。坐标字段保留（可观测性 + 旧 runtime
的 RBAC 回退）。

### Operator 注入

- 所有 Agent runtime Deployment 注入 `AGENTPLANE_MODEL_API_KEY`
  （`valueFrom: secretKeyRef`，来源 = 已有的 `resolveModelSecret` 解析结果）。
- 解析 best-effort（同 `resolveModelSecret` 现状）：引用的 CR 未就绪 →
  Agent 已 Degraded，不阻塞 Deployment 存在。
- 注入 env 追加在 `spec.runtime.env` **之后**（Operator 赢，同
  `AGENTPLANE_CREDENTIALS_PATH` 的先例，agent_controller.go:964-969）。
- 保留名：`ReservedRuntimeEnv`（api/v1alpha1/validation.go:339）增加
  `AGENTPLANE_MODEL_API_KEY`（静态名，无动态推导）。
- Secret 不存在：pod 无法启动——与现有文件挂载同一失败类别，由 pod status
  显式报错。不设 `optional`。

### 协议义务改写（docs/runtime-protocol.md）

- §1.3：坐标 + env 名并载；值经 env 到达 runtime。
- §6 义务："MUST 自备 RBAC 读 Secret" → "MUST 从 `payload.*.env` 命名的
  环境变量取值；在 Operator 管辖下 MUST NOT 需要 kube client；MAY 在 env
  缺席时回退坐标 + RBAC"（集群外运行、或旧 Registry 的过渡场景）。
- 附 runtime 刚需 env 表，作为协议的一部分写明：
  - 无条件：`AGENTPLANE_REGISTRY`、`AGENTPLANE_AGENT_NAME`、
    `AGENTPLANE_AGENT_NAMESPACE`；
  - 按需：payload 命名的 secret env；
  - coding-agent 布局：`HOME`/`XDG_*`/`AGENTPLANE_WORKSPACE`/
    `AGENTPLANE_GIT_CREDENTIAL_FILE`/`GIT_CONFIG_*`/`AGENTPLANE_CREDENTIALS_PATH`。
- §3 payload 示例补 `env` 字段。

### SDK / runtime 消费方

（2026-09-03 更新：cmd/agent-runtime 与 cmd/node-agent-runtime 已按
remove-legacy-runtimes 设计删除。）env-first 的首个实现者是 **pi
runtime**；SDK 保留 `Env` 字段与"env 优先、坐标 + RBAC 回退"的读取
helper，供集群外/旧 Registry 场景与第三方 runtime 使用。

### pi runtime（动机落点）

宿主进程 `registerProvider({ apiKey: "$AGENTPLANE_MODEL_API_KEY", baseUrl })`
—— pi 的 `$ENV` 插值直接消费注入变量，**零行读 secret 的代码、零 kube
client**。配套进程开关：`PI_OFFLINE`、`PI_SKIP_VERSION_CHECK`、
`PI_TELEMETRY`（离线集群防启动期网络操作）。

### 轮换语义

env 值固定于 pod 生命周期：Secret 轮换需要 rollout。现状 configHash 本就
不含 Secret 值变化，RBAC 路径同样读不到新值直到下一次 reload——不是退化，
但从含糊改为协议里写死。

## 错误处理

- payload.env 缺失（旧 Registry）→ runtime 回退 RBAC 路径。
- 注入 env 存在但值为空（Secret 键为空）→ runtime 报 credential empty，
  与现状一致。
- best-effort 解析失败 → 不阻塞 Deployment；Agent Degraded 已覆盖。

## 验证

1. envtest：注入 env 的名字与 secretKeyRef 来源正确；`spec.runtime.env`
   碰撞 `AGENTPLANE_MODEL_API_KEY` 在 apply 时被拒。
2. Registry 单测：payload.model.env 字段存在且值为该常量。
3. e2e：payload.env 报告的名字 == pod 实际注入的 env（防 Operator/Registry
   两侧漂移）。

## 测试

- 上述 1–3。轮换语义只文档化，不做自动化测试。
