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
# Fully-qualified on purpose: podman normalizes unqualified names to
# localhost/... in the node's containerd, which then no longer matches what
# kubelet resolves (docker.io/library/...) — IfNotPresent misses and the pull
# hits docker.io and fails.
IMG="${IMG:-docker.io/library/agent-plane:dev}"
REGISTRY_IMG="${REGISTRY_IMG:-docker.io/library/agent-plane-registry:dev}"  # resolves same as the unqualified name in config/registry/registry.yaml
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
  check_distinct_images "$IMG" "$REGISTRY_IMG"
}

# kind+podman known issue: if a re-tag races (see kind's "already present but
# missing the tag, re-tagging..." log line) two distinct images can end up
# sharing one node-side image ID — the container that starts is whichever
# image "won", silently. Cheap to catch: compare crictl's reported IDs.
check_distinct_images() {
  detect_provider
  local ids
  ids="$(docker exec "${KIND_CLUSTER}-control-plane" crictl images 2>/dev/null | \
    awk -v a="${1%%:*}" -v b="${2%%:*}" '$1==a || $1==b {print $3}' | sort -u | wc -l)"
  if [ "$ids" -lt 2 ]; then
    echo "!!! $1 and $2 resolved to the same image ID on the node — reload with:" >&2
    echo "    docker exec ${KIND_CLUSTER}-control-plane crictl rmi $1 $2; $0 images" >&2
    exit 1
  fi
}

cmd_cert_manager() {
  if kubectl get crd certificates.cert-manager.io >/dev/null 2>&1; then
    echo ">>> cert-manager already installed"
  else
    kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"
  fi
  kubectl wait --for=condition=Available deployment --all -n cert-manager --timeout="$WAIT_TIMEOUT"
}

# Backups for cmd_apply. Deliberately global: an EXIT trap cannot see a
# function's locals (they are gone by the time the trap runs), so a local-based
# restore silently failed and left the kustomization edits behind on error.
APPLY_BACKUPS=()

cmd_apply() {
  echo ">>> applying config/default with image $IMG"
  local mgr="config/manager/kustomization.yaml" reg="config/registry/kustomization.yaml"
  local mgr_backup reg_backup
  mgr_backup="$(mktemp)"
  reg_backup="$(mktemp)"
  cp "$mgr" "$mgr_backup"
  cp "$reg" "$reg_backup"
  APPLY_BACKUPS=("$mgr_backup:$mgr" "$reg_backup:$reg")
  trap 'for pair in "${APPLY_BACKUPS[@]}"; do mv -f "${pair%%:*}" "${pair#*:}"; done' EXIT
  (cd config/manager && "$KUSTOMIZE_BIN" edit set image controller="$IMG")
  "$KUSTOMIZE_BIN" build config/default | kubectl apply -f -
  echo ">>> applying config/registry with image $REGISTRY_IMG"
  (cd config/registry && "$KUSTOMIZE_BIN" edit set image agent-plane-registry="$REGISTRY_IMG")
  "$KUSTOMIZE_BIN" build config/registry | kubectl apply -f -
  # Same dev tag on every build: apply alone sees no template diff and keeps
  # the old pods. Restart to pick up the freshly loaded image.
  kubectl rollout restart deployment/agent-plane-controller-manager deployment/agent-plane-registry -n "$NAMESPACE"
  for pair in "${APPLY_BACKUPS[@]}"; do mv -f "${pair%%:*}" "${pair#*:}"; done
  APPLY_BACKUPS=()
  trap - EXIT
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
    awk 'NR>1 && /^#/{sub(/^# ?/,""); print; next} NR>1{exit}' "$0"
    exit 1
    ;;
esac
