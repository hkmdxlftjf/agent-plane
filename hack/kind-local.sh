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
    awk 'NR>1 && /^#/{sub(/^# ?/,""); print; next} NR>1{exit}' "$0"
    exit 1
    ;;
esac
