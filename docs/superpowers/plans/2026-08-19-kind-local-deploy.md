# 本地 kind 一键部署 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 提供 `make deploy-kind` 一键在专用本地 kind 集群部署控制面全量（Operator + Registry + cert-manager + validating webhooks），并修复 webhook 生产路径缺口。

**Architecture:** `hack/kind-local.sh` 承载全部过程逻辑（集群生命周期、镜像构建加载、cert-manager 安装、部署、验证），Makefile 目标薄封装。webhook 缺口通过把 `ENABLE_WEBHOOKS=false` 从 `config/manager/manager.yaml`（生产基座）移到 `config/local` overlay（本地无 webhook 路径）修复。

**Tech Stack:** bash、kind v0.32、kustomize v5.7.1（bin/kustomize）、kubectl、cert-manager v1.18.2、podman（本机 `docker` 为 podman 别名，需 `KIND_EXPERIMENTAL_PROVIDER=podman`）。

**Spec:** `docs/superpowers/specs/2026-08-19-kind-local-deploy-design.md`

---

### Task 1: 修复 webhook 生产路径缺口（config 层）

**背景：** `config/manager/manager.yaml` 硬编码 `ENABLE_WEBHOOKS=false`，而 `config/default` 部署 ValidatingWebhookConfiguration 并由 cert-manager 注入 CA —— CA 注入完成的瞬间，所有 Agent/Workflow/Tool 的写请求会被路由到不存在的 webhook server 而失败。

**Files:**
- Modify: `config/manager/manager.yaml:68-73`（删除 env 块）
- Create: `config/local/manager-disable-webhooks.yaml`（patch）
- Modify: `config/local/kustomization.yaml`（引用 patch + 更新注释）

- [ ] **Step 1: 从 config/manager/manager.yaml 删除 ENABLE_WEBHOOKS env**

将 `containers[0]` 的：

```yaml
        env:
          # Webhooks require TLS certs (cert-manager). Disabled here so the
          # manager runs without a webhook server; re-enable once cert-manager
          # and the webhook/cert-manager kustomize components are installed.
          - name: ENABLE_WEBHOOKS
            value: "false"
```

整块删除（`ports: []` 保留）。

- [ ] **Step 2: 新建 config/local/manager-disable-webhooks.yaml**

```yaml
# Local overlay only: the manager runs without a webhook server (this overlay
# ships no ValidatingWebhookConfiguration and no cert-manager), so instruct the
# binary accordingly. Production (config/default) enables webhooks.
- op: add
  path: /spec/template/spec/containers/0/env
  value:
    - name: ENABLE_WEBHOOKS
      value: "false"
```

- [ ] **Step 3: config/local/kustomization.yaml 引用 patch**

在文件末尾追加，并把头部注释里 "see config/manager/manager.yaml" 的说明改为指向本 overlay 的 patch：

```yaml
patches:
- path: manager-disable-webhooks.yaml
  target:
    kind: Deployment
    name: controller-manager
```

- [ ] **Step 4: kustomize 构建验证（充当测试）**

```sh
bin/kustomize build config/local   | grep -c ENABLE_WEBHOOKS   # 期望输出: 1
bin/kustomize build config/default | grep -c ENABLE_WEBHOOKS   # 期望输出: 0
bin/kustomize build config/default | grep -c webhook-server-cert  # 期望输出: >=2（mount+volume）
```

- [ ] **Step 5: Commit**

```sh
git add config/manager/manager.yaml config/local/
git commit -m "fix(config): enable webhooks in production, disable only in local overlay"
```

---

### Task 2: hack/kind-local.sh 脚本

**Files:**
- Create: `hack/kind-local.sh`（`chmod +x`）

- [ ] **Step 1: 写入完整脚本**

```bash
#!/usr/bin/env bash
# kind-local.sh — manage a dedicated local kind cluster for agent-plane.
#
# Subcommands:
#   create        create the cluster (reuse if it exists) and switch kubeconfig
#   delete        delete the cluster
#   images        build and load operator + registry images into the cluster
#   cert-manager  install cert-manager (reuse if present) and wait for readiness
#   apply         apply config/default (operator) + config/registry
#   verify        wait for pods, check webhook CA injection
#   test-webhook  apply a valid Workflow (accepted) and an invalid one (denied)
#   deploy        one-shot: create + images + cert-manager + apply + verify + test-webhook
#
# Override with env vars: KIND_CLUSTER, IMG, REGISTRY_IMG, CERT_MANAGER_VERSION, WAIT_TIMEOUT.
set -euo pipefail

KIND_CLUSTER="${KIND_CLUSTER:-agent-plane}"
IMG="${IMG:-agent-plane:dev}"
REGISTRY_IMG="${REGISTRY_IMG:-agent-plane-registry:dev}"  # must match config/registry/registry.yaml
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.18.2}"
NAMESPACE="agent-plane-system"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-300s}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

KUSTOMIZE_BIN="$REPO_ROOT/bin/kustomize"
[ -x "$KUSTOMIZE_BIN" ] || KUSTOMIZE_BIN="kustomize"

# `docker` may be a podman alias; kind then needs the experimental provider flag.
detect_provider() {
  if docker info 2>/dev/null | grep -qi podman; then
    export KIND_EXPERIMENTAL_PROVIDER=podman
  fi
}

kindctl() { detect_provider && kind "$@"; }

cluster_exists() {
  kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"
}

cmd_create() {
  if cluster_exists; then
    echo ">>> kind cluster '$KIND_CLUSTER' already exists, reusing"
  else
    echo ">>> creating kind cluster '$KIND_CLUSTER'"
    kindctl create cluster --name "$KIND_CLUSTER"
  fi
  kubectl config use-context "kind-$KIND_CLUSTER"
}

cmd_delete() {
  kindctl delete cluster --name "$KIND_CLUSTER"
}

cmd_images() {
  echo ">>> building $IMG and $REGISTRY_IMG"
  docker build -t "$IMG" -f Dockerfile .
  docker build -t "$REGISTRY_IMG" -f Dockerfile.registry .
  echo ">>> loading images into kind"
  kindctl load docker-image "$IMG" --name "$KIND_CLUSTER"
  kindctl load docker-image "$REGISTRY_IMG" --name "$KIND_CLUSTER"
}

cmd_cert_manager() {
  if kubectl get crd certificates.cert-manager.io >/dev/null 2>&1; then
    echo ">>> cert-manager already installed"
  else
    kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"
  fi
  kubectl wait --for=condition=Available deployment --all -n cert-manager --timeout="$WAIT_TIMEOUT"
}

cmd_apply() {
  echo ">>> applying config/default with image $IMG"
  local mgr="config/manager/kustomization.yaml" backup
  backup="$(mktemp)"
  cp "$mgr" "$backup"
  trap 'mv "$backup" "$mgr"' EXIT
  (cd config/manager && "$KUSTOMIZE_BIN" edit set image controller="$IMG")
  "$KUSTOMIZE_BIN" build config/default | kubectl apply -f -
  mv "$backup" "$mgr"
  trap - EXIT
  echo ">>> applying config/registry"
  kubectl apply -f config/registry/registry.yaml
}

cmd_verify() {
  kubectl wait --for=condition=Available deployment/agent-plane-controller-manager \
    -n "$NAMESPACE" --timeout="$WAIT_TIMEOUT"
  kubectl wait --for=condition=Available deployment/agent-plane-registry \
    -n "$NAMESPACE" --timeout="$WAIT_TIMEOUT"
  local cab
  cab="$(kubectl get validatingwebhookconfiguration \
    agent-plane-validating-webhook-configuration \
    -o jsonpath='{.webhooks[0].clientConfig.caBundle}')"
  if [ "${#cab}" -lt 10 ]; then
    echo "!!! webhook CA bundle not injected" >&2
    exit 1
  fi
  echo ">>> ready:"
  kubectl get pods -n "$NAMESPACE"
  echo ">>> registry access:"
  echo "    kubectl port-forward -n $NAMESPACE deploy/agent-plane-registry 9090:9090"
  echo "    curl localhost:9090/v1/agents/<ns>/<name>/config"
}

cmd_test_webhook() {
  echo ">>> applying a valid Workflow (expect: accepted)"
  kubectl apply -f - <<'EOF'
apiVersion: core.hkmdxlftjf.io/v1alpha1
kind: Workflow
metadata:
  name: kind-local-smoke
spec:
  engine: agentloop
  version: "1.0.0"
  steps:
    - name: done
      type: finish
EOF
  echo ">>> applying an invalid Workflow, duplicate step names (expect: denied)"
  if kubectl apply -f - <<'EOF'
apiVersion: core.hkmdxlftjf.io/v1alpha1
kind: Workflow
metadata:
  name: kind-local-smoke-bad
spec:
  engine: agentloop
  version: "1.0.0"
  steps:
    - name: dup
      type: planner
    - name: dup
      type: finish
EOF
  then
    echo "!!! invalid Workflow was accepted — webhook not enforcing" >&2
    kubectl delete workflow kind-local-smoke kind-local-smoke-bad --ignore-not-found
    exit 1
  fi
  echo ">>> denied as expected"
  kubectl delete workflow kind-local-smoke --ignore-not-found
}

cmd_deploy() {
  cmd_create
  cmd_images
  cmd_cert_manager
  cmd_apply
  cmd_verify
  cmd_test_webhook
  echo ">>> deploy complete"
}

case "${1:-}" in
  create) cmd_create ;;
  delete) cmd_delete ;;
  images) cmd_images ;;
  cert-manager) cmd_cert_manager ;;
  apply) cmd_apply ;;
  verify) cmd_verify ;;
  test-webhook) cmd_test_webhook ;;
  deploy) cmd_deploy ;;
  *)
    grep '^#' "$0" | sed 's/^# \{0,1\}//;s/^kind-local.sh — //'
    exit 1
    ;;
esac
```

- [ ] **Step 2: chmod +x 并做语法检查**

```sh
chmod +x hack/kind-local.sh
bash -n hack/kind-local.sh   # 期望: 无输出
```

- [ ] **Step 3: Commit**

```sh
git add hack/kind-local.sh
git commit -m "feat(dev): add hack/kind-local.sh for dedicated local kind cluster management"
```

---

### Task 3: Makefile 目标

**Files:**
- Modify: `Makefile`（Deployment 段，`undeploy` 目标之后）

- [ ] **Step 1: 添加目标**

```makefile
##@ Local kind cluster

KIND_CLUSTER ?= agent-plane

.PHONY: kind-create
kind-create: ## Create (or reuse) the dedicated local kind cluster.
	KIND_CLUSTER=$(KIND_CLUSTER) ./hack/kind-local.sh create

.PHONY: kind-delete
kind-delete: ## Delete the dedicated local kind cluster.
	KIND_CLUSTER=$(KIND_CLUSTER) ./hack/kind-local.sh delete

.PHONY: kind-images
kind-images: ## Build and load operator + registry images into the kind cluster.
	KIND_CLUSTER=$(KIND_CLUSTER) ./hack/kind-local.sh images

.PHONY: deploy-kind
deploy-kind: ## One-shot local deploy: kind + cert-manager + operator + registry (full webhook path).
	KIND_CLUSTER=$(KIND_CLUSTER) ./hack/kind-local.sh deploy
```

- [ ] **Step 2: dry-run 验证**

```sh
make -n deploy-kind KIND_CLUSTER=agent-plane   # 期望: 展开为 ./hack/kind-local.sh deploy
```

- [ ] **Step 3: Commit**

```sh
git add Makefile
git commit -m "feat(dev): add make targets for the local kind workflow"
```

---

### Task 4: 实际部署与验证

- [ ] **Step 1: 执行一键部署**

```sh
make deploy-kind
```

预期：集群创建 → 两镜像构建加载（Go 构建，可能 5-15 分钟）→ cert-manager 就绪 → operator/registry Applied → Pods Ready → smoke Workflow 通过（合法 accepted / 非法 denied）。

失败排查提示：`kubectl -n agent-plane-system describe pod`、`kubectl -n cert-manager get pods`、`kubectl get validatingwebhookconfiguration -o yaml`。

- [ ] **Step 2: 确认最终状态**

```sh
kubectl -n agent-plane-system get pods,svc
kubectl -n agent-plane-system get deploy agent-plane-controller-manager \
  -o jsonpath='{.spec.template.spec.containers[0].env}'   # 期望: 不含 ENABLE_WEBHOOKS
```

- [ ] **Step 3: 清理能力验证（可选，验证后可重建）**

```sh
make kind-delete && kind get clusters   # agent-plane 消失，其他集群不受影响
make deploy-kind                        # 从零重建成功（幂等性证明）
```

---

### Task 5: 部署后优化评审（原需求第二半）

- [ ] **Step 1: 检查部署产物并汇总优化点**

检查项（逐条核对，输出报告，未经用户同意不改代码）：

```sh
docker images | grep agent-plane          # 镜像体积
kubectl -n agent-plane-system get deploy -o yaml | grep -A4 resources
kubectl -n agent-plane-system top pods    # 实际用量 vs requests/limits
```

已知候选（验证后写入报告）：
1. webhook 生产路径缺口（Task 1 已修复）—— 作为已落地项记录。
2. `Dockerfile`/`Dockerfile.registry` builder 均为 `golang:1.26` 全量镜像，无 build cache 挂载，重复构建慢。
3. `config/registry/registry.yaml` 硬编码镜像 tag `agent-plane-registry:dev`，与 Makefile 无变量联动。
4. Registry Deployment 无 probes（manager 有 liveness/readiness，registry 无任何探针）。
5. `make deploy` 通过 `kustomize edit` 原地修改仓库文件（本脚本已用备份规避，根治需 overlay 化）。

- [ ] **Step 2: 向用户报告优化清单，等待取舍**
