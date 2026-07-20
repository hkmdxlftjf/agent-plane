#!/usr/bin/env bash
# Open the deployed agent's web UI (port-forwards the runtime Service).
# Usage: bash hack/web.sh [namespace] [agent-name]   (defaults: agent-plane-demo support-agent)
set -uo pipefail
export KUBECONFIG="${KUBECONFIG_OVERRIDE:-$HOME/.kube/config}"
NS="${1:-agent-plane-demo}"
AGENT="${2:-support-agent}"
echo "cluster: $(kubectl config current-context)  ns: $NS  agent: $AGENT"
echo "▶ open  http://localhost:8080  in your browser (Ctrl-C to stop)"
exec kubectl -n "$NS" port-forward "svc/${AGENT}-runtime" 8080:8080
