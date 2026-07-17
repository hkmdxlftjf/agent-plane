#!/usr/bin/env bash
# Interactive chat with a deployed CogNet agent.
#
# Sets up port-forwards to the in-cluster Registry and the agent's MCP tool,
# then launches an interactive multi-turn chat (tool calls happen transparently).
# Runs on your host against the deployed Agent; the model key is read from the
# referenced Secret via your kubeconfig.
#
# Usage:  bash hack/chat.sh [namespace] [agent-name]
#   defaults: namespace=cognet-demo agent=support-agent
set -uo pipefail

export KUBECONFIG="${KUBECONFIG_OVERRIDE:-$HOME/.kube/config}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"
NS="${1:-cognet-demo}"
AGENT="${2:-support-agent}"

echo "cluster: $(kubectl config current-context)  ns: $NS  agent: $AGENT"

REG_PID=""; MCP_PID=""
cleanup() { [[ -n "$REG_PID" ]] && kill "$REG_PID" 2>/dev/null; [[ -n "$MCP_PID" ]] && kill "$MCP_PID" 2>/dev/null; }
trap cleanup EXIT

echo "▶ port-forwarding Registry (9090) and MCP tool (18080)…"
kubectl -n agent-plane-system port-forward svc/cognet-registry 9090:9090 >/tmp/cognet-chat-reg.log 2>&1 &
REG_PID=$!
# The demo agent's tool is order-lookup → orders-mcp; forward it so the host can reach it.
if kubectl -n "$NS" get svc orders-mcp >/dev/null 2>&1; then
  kubectl -n "$NS" port-forward svc/orders-mcp 18080:8080 >/tmp/cognet-chat-mcp.log 2>&1 &
  MCP_PID=$!
fi
# wait for the registry forward to be ready
for _ in $(seq 1 20); do curl -sf localhost:9090/healthz >/dev/null 2>&1 && break; sleep 0.5; done
sleep 1

echo "▶ starting chat…"
go run ./cmd/agent-runtime --chat \
  --registry http://localhost:9090 \
  --namespace "$NS" --name "$AGENT" \
  --tool-endpoint order-lookup=http://localhost:18080 \
  --tool-endpoint weather=http://localhost:18080
