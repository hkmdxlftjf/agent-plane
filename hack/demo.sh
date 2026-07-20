#!/usr/bin/env bash
# One-click Agent Plane Agent demo.
#
# Stands up a complete Agent (Model + Prompt + MCP Tool + Skill) in the
# `agent-plane-demo` namespace, then runs the real agent-runtime against it so you
# can watch a live model call a real MCP tool — all driven by Agent Plane config.
#
# Prerequisites:
#   * The Agent Plane operator is deployed (make deploy / config/local) and running.
#   * An LLM credential in your env: either
#       - ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN (OpenAI-compatible gateway), or
#       - OPENROUTER_API_KEY
#   * Docker + a local cluster that shares its image store (OrbStack/kind/minikube).
#
# Usage:
#   bash hack/demo.sh ["your prompt"]
#   KEEP=1 bash hack/demo.sh        # keep the demo resources after running
#   DEMO_MODEL=claude-opus-4-6 bash hack/demo.sh
set -uo pipefail

export KUBECONFIG="${KUBECONFIG_OVERRIDE:-$HOME/.kube/config}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"
NS=agent-plane-demo
PROMPT="${1:-What is the delivery status of order A-42? Give me the carrier and ETA.}"

green() { printf '\033[32m%s\033[0m\n' "$1"; }
red()   { printf '\033[31m%s\033[0m\n' "$1"; }
step()  { printf '\n\033[36m▶ %s\033[0m\n' "$1"; }

REGISTRY_PID=""
PF_PID=""
cleanup() {
  [[ -n "$REGISTRY_PID" ]] && kill "$REGISTRY_PID" 2>/dev/null
  [[ -n "$PF_PID" ]] && kill "$PF_PID" 2>/dev/null
  if [[ "${KEEP:-0}" == "1" ]]; then
    green "KEEP=1 — demo resources left in namespace $NS (delete with: kubectl delete ns $NS)"
  else
    kubectl delete ns "$NS" --wait=false >/dev/null 2>&1
    green "cleaned up namespace $NS"
  fi
}
trap cleanup EXIT

# --- 0. preflight ------------------------------------------------------------
step "Preflight"
echo "cluster: $(kubectl config current-context)"
if ! kubectl -n agent-plane-system get deploy agent-plane-controller-manager >/dev/null 2>&1; then
  red "Agent Plane operator not found in agent-plane-system. Deploy it first (see README)."
  exit 1
fi

# Choose model backend from whatever credential is available.
if [[ -n "${ANTHROPIC_BASE_URL:-}" && -n "${ANTHROPIC_AUTH_TOKEN:-}" ]]; then
  PROVIDER=custom
  ENDPOINT="${ANTHROPIC_BASE_URL%/}/v1"
  MODEL="${DEMO_MODEL:-claude-haiku-4-5-20251001}"
  APIKEY="$ANTHROPIC_AUTH_TOKEN"
  echo "model backend: gateway ($ENDPOINT) model=$MODEL"
elif [[ -n "${OPENROUTER_API_KEY:-}" ]]; then
  PROVIDER=openrouter
  ENDPOINT="https://openrouter.ai/api/v1"
  MODEL="${DEMO_MODEL:-openai/gpt-4o-mini}"
  APIKEY="$OPENROUTER_API_KEY"
  echo "model backend: openrouter model=$MODEL"
else
  red "No LLM credential in env. Set ANTHROPIC_BASE_URL+ANTHROPIC_AUTH_TOKEN or OPENROUTER_API_KEY."
  exit 1
fi

# --- 1. build the example MCP image ------------------------------------------
step "Building example MCP server image"
if ! docker image inspect agent-plane-example-mcp:dev >/dev/null 2>&1; then
  docker build -f Dockerfile.example-mcp -t agent-plane-example-mcp:dev . >/dev/null && green "built agent-plane-example-mcp:dev"
else
  green "agent-plane-example-mcp:dev already present"
fi

# --- 2. ensure CRDs are current, create namespace + env-specific resources ---
step "Applying CRDs + demo resources"
kubectl apply -k config/crd >/dev/null
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" create secret generic llm-secret --from-literal=api-key="$APIKEY" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: core.hkmdxlftjf.io/v1alpha1
kind: Model
metadata: {name: llm-model, namespace: $NS}
spec:
  provider: $PROVIDER
  modelName: "$MODEL"
  endpoint: "$ENDPOINT"
  credentialRef: {name: llm-cred}
EOF
kubectl apply -k config/demo >/dev/null
green "applied Model + Credential + MCPServer + Tool + Skill + PromptTemplate + Agent"

# --- 3. wait for readiness ---------------------------------------------------
step "Waiting for resources to reconcile"
kubectl -n "$NS" rollout status deploy/orders-mcp --timeout=120s
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready agent/support-agent --timeout=60s
kubectl -n "$NS" get agent support-agent -o jsonpath='  agent phase={.status.phase} refs={.status.resolvedRefs}{"\n"}'
kubectl -n "$NS" get tool/order-lookup mcpserver/orders-mcp skill/refund-handling

# --- 4. start Registry + port-forward to the in-cluster MCP ------------------
step "Starting Registry (data plane) + port-forward to MCP"
KUBECONFIG="$KUBECONFIG" go run ./cmd/registry --addr :9090 >/tmp/agent-plane-demo-registry.log 2>&1 &
REGISTRY_PID=$!
KUBECONFIG="$KUBECONFIG" kubectl -n "$NS" port-forward svc/orders-mcp 18080:8080 >/tmp/agent-plane-demo-pf.log 2>&1 &
PF_PID=$!
# wait for registry
for _ in $(seq 1 30); do curl -sf localhost:9090/healthz >/dev/null 2>&1 && break; sleep 1; done
sleep 2
green "registry up (pid $REGISTRY_PID), port-forward up (pid $PF_PID)"

# --- 5. run the real agent ---------------------------------------------------
step "Running the agent (real model + real MCP tool call)"
go run ./cmd/agent-runtime \
  --registry http://localhost:9090 \
  --namespace "$NS" --name support-agent \
  --tool-endpoint order-lookup=http://localhost:18080 \
  --prompt "$PROMPT"
