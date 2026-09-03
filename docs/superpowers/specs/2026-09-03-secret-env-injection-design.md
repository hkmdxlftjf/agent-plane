# Design: Secret 经环境变量注入（runtime protocol v1.1）

Date: 2026-09-03

## 背景

v1 协议（docs/runtime-protocol.md §1.3/§6）规定 payload 只携带 Secret 坐标
（secretName/secretKey），runtime **必须**自备 Kubernetes RBAC 读值。后果：

- 文档自己记录的 footgun：忘记绑 `get secrets` Role → pod 正常启动、每轮
  请求失败（§1.3）。
- 每个 runtime 都要带一个 kube client：Go SDK 有 `secrets.NewReader`；
  node-agent-runtime 手写了 57 行 in-cluster client
  （cmd/node-agent-runtime/src/secrets.js）只为把坐标换成值；pi 迁移评估
  中这是明确成本项——且 kube API 凭据住在跑模型 shell 命令的同一进程里
  （agent_controller.go:552-562 的注释已为此把 model secret 挂成了文件）。

"值不走 RBAC"在 repo 里已有三条先例（model secret 文件挂载、Credential CR
文件挂载、git credential 文件挂载）。本设计把这条线走完：**env 注入成为
协议的一等路径，RBAC 读取降级为回退**。

## 目标

- 所有 Agent runtime pod：model / memories / knowledgeBases 的 Secret 以
  `valueFrom: secretKeyRef` 注入 env，变量名由 Registry payload 携带。
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
| `memories[i]` | `AGENTPLANE_MEMORY_<NAME>_KEY` |
| `knowledgeBases[i]` | `AGENTPLANE_KB_<NAME>_KEY` |

`<NAME>` 为 CR 名经 sanitize（大写、`-`→`_`）。CR 名是 DNS-1123（小写
字母数字加连字符），到 env 片段的映射是单射，不会碰撞。规则实现为一个
共享 helper，Registry（写进 payload）与 Operator（注入 env）两侧共用，
e2e 断言两侧一致；SDK 不需要规则——读 `payload.*.env` 即可。

### AgentConfig 增字段（additive，符合 v1 前向兼容）

`model.env`、`memories[i].env`、`knowledgeBases[i].env`：string，注入的
变量名。坐标字段保留（可观测性 + 旧 runtime 的 RBAC 回退）。

### Operator 注入

- 所有 Agent runtime Deployment 注入上述 env（`valueFrom: secretKeyRef`），
  与 Registry 同一命名规则。
- 解析 best-effort（同 `resolveModelSecret` 现状）：引用的 CR 未就绪 →
  Agent 已 Degraded，不阻塞 Deployment 存在。
- 注入 env 追加在 `spec.runtime.env` **之后**（Operator 赢，同
  `AGENTPLANE_CREDENTIALS_PATH` 的先例，agent_controller.go:964-969）。
- 保留名：`ReservedRuntimeEnv`（api/v1alpha1/validation.go:339）增加静态名
  `AGENTPLANE_MODEL_API_KEY`；动态名按 Agent 自身 `spec.memoryRefs` /
  `spec.knowledgeBaseRefs` 在 apply 时推导校验（spec 上可得，不需要跨对象
  存在性）。
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

### SDK / 参考 runtime / node-agent-runtime

读取顺序统一为：payload.env 命名的环境变量 → 回退坐标 + RBAC（日志注明
走了哪条）。

- cmd/agent-runtime：改读 env，`secrets.NewReader` 留作回退。
- cmd/node-agent-runtime：secrets.js 降级为回退路径（本地开发仍需）。
- agent-plane-sdk-go：payload 类型增 `Env` 字段；可加便捷读取 helper。

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
   碰撞静态/动态保留名在 apply 时被拒。
2. Registry 单测：payload 含 env 字段且命名符合规则。
3. e2e：payload.env 报告的名字 == pod 实际注入的 env（防 Operator/Registry
   两侧规则漂移）。
4. 参考 runtime：env 路径取值成功；env 缺席时回退 RBAC 成功。

## 测试

- 上述 1–4。轮换语义只文档化，不做自动化测试。
