# Design: 本地 kind 一键部署（控制面全量 + cert-manager）

Date: 2026-08-19

## 目标

为 agent-plane 提供专用的本地 kind 集群一键部署能力：Operator + Registry
（控制面全量），走 cert-manager + validating webhooks 的完整生产路径。
不污染机器上其他项目的 kind 集群。

## 非目标

- 部署数据面示例（example-mcp、agent-runtime、lark adapter 等）。
- 多节点 / 自定义拓扑的 kind 配置。
- 推送镜像到外部 registry。

## 方案（已选 B：hack 脚本 + Makefile 薄封装）

### 组件

- `hack/kind-local.sh` — 子命令：`create` / `delete` / `images` /
  `cert-manager` / `deploy` / `verify`。
  - 变量：`KIND_CLUSTER=agent-plane`、`IMG=agent-plane:dev`、
    `REGISTRY_IMG=agent-plane-registry:dev`、
    `CERT_MANAGER_VERSION=v1.18.2`（与 test/utils/utils.go 一致）。
  - provider 探测：`docker info` 输出含 podman 时导出
    `KIND_EXPERIMENTAL_PROVIDER=podman`（本机 docker 为 podman 别名）。
  - `set -euo pipefail`；所有 `kubectl wait` 带 `--timeout`；幂等可重入。
- Makefile 目标：`kind-create` / `kind-delete` / `kind-images` /
  `deploy-kind`（编排：create → images → cert-manager → deploy → verify）。

### deploy-kind 流程

1. 集群不存在则 `kind create cluster`（存在则复用），切换 kubecontext。
2. 构建 controller（Dockerfile）与 registry（Dockerfile.registry）镜像，
   `kind load` 进集群。
3. 安装 cert-manager v1.18.2，等待三个 Deployment 就绪。
4. `make deploy IMG=agent-plane:dev`（config/default 全量）+
   `kubectl apply -f config/registry`。
5. verify：等待 controller-manager 与 registry Pod Ready；检查
   ValidatingWebhookConfiguration 的 caBundle 已注入。成功后打印
   registry port-forward 用法提示。

### 顺带修复：webhook 生产路径缺口

`config/manager/manager.yaml` 硬编码 `ENABLE_WEBHOOKS=false`，而
`config/default` 会部署 ValidatingWebhookConfiguration 且 cert-manager
会注入 CA——结果 manager 不起 webhook server，而 API server 已开始把
Agent/Workflow/Tool 的写请求路由给不存在的服务端，全部 apply 失败。

修复：

- `config/manager/manager.yaml`：移除 `ENABLE_WEBHOOKS=false`（生产默认开）。
- `config/local/kustomization.yaml`：新增 patch 注入
  `ENABLE_WEBHOOKS=false`（本地 overlay 本就不含 webhook 组件，语义不变）。

## 错误处理

- 脚本每步失败即停，打印失败步骤名。
- `kubectl wait` 统一 `--timeout=180s`（cert-manager webhook 首次拉镜像可能慢）。
- `deploy` 幂等：kind create 检查存在性；kubectl apply 天然幂等；
  cert-manager 安装前检测 CRD 是否已存在。

## 验证

1. `make deploy-kind` 全流程成功。
2. controller-manager、registry Pod Ready；webhook caBundle 非空。
3. apply 一个最小合法 Workflow CR 成功（证明 admission 路径通），再 apply
   一个非法（重复 step 名）被 webhook 拒绝，随后清理。
4. `make kind-delete` 可完整回收。

## 测试

- 手动执行上述验证步骤（本设计的主体是部署脚本，无单测；
  webhook 修复由现有 envtest 覆盖配置无关，不需新测试）。
