#!/usr/bin/env bash
# Functional test of the deployed CogNet operator against the live cluster.
# Runs in an isolated namespace and asserts real controller behavior.
set -uo pipefail
export KUBECONFIG="$HOME/.kube/config"
NS=cognet-func-test
K="kubectl -n $NS"

PASS=0; FAIL=0
green() { printf '\033[32m%s\033[0m\n' "$1"; }
red()   { printf '\033[31m%s\033[0m\n' "$1"; }

# expect_eventually "desc" "jsonpath-query on a resource" "expected" [timeout_s]
expect_eventually() {
  local desc="$1" get="$2" want="$3" timeout="${4:-30}"
  local got=""
  for ((i=0; i<timeout*2; i++)); do
    got=$(eval "$get" 2>/dev/null)
    [[ "$got" == "$want" ]] && { green "PASS: $desc (=$got)"; ((PASS++)); return; }
    sleep 0.5
  done
  red "FAIL: $desc (want '$want', got '$got')"; ((FAIL++))
}

echo "### setup"
kubectl delete ns $NS --ignore-not-found --wait=true >/dev/null 2>&1
kubectl create ns $NS >/dev/null

########################################################################
echo; echo "### Scenario A: Agent reference resolution + auto-convergence via watch"
cat <<'EOF' | $K apply -f - >/dev/null
apiVersion: core.cognet.io/v1alpha1
kind: Agent
metadata: {name: a1}
spec:
  modelRef: {name: m1}
EOF
expect_eventually "A1 Agent with missing Model -> Degraded" \
  "$K get agent a1 -o jsonpath='{.status.phase}'" "Degraded"

# Create the missing Model; the Agent watches Models, so it should auto-reconcile.
cat <<'EOF' | $K apply -f - >/dev/null
apiVersion: core.cognet.io/v1alpha1
kind: Model
metadata: {name: m1}
spec: {provider: anthropic, modelName: claude-opus-4-8}
EOF
expect_eventually "A2 Agent auto-converges to Ready after Model created (watch works)" \
  "$K get agent a1 -o jsonpath='{.status.phase}'" "Ready"
expect_eventually "A3 Agent publishes a resolvedConfigHash" \
  "$K get agent a1 -o jsonpath='{.status.resolvedConfigHash}' | cut -c1-8 | wc -c | tr -d ' '" "9"

########################################################################
echo; echo "### Scenario B: Credential verifies backing Secret (watches Secrets)"
cat <<'EOF' | $K apply -f - >/dev/null
apiVersion: core.cognet.io/v1alpha1
kind: Credential
metadata: {name: c1}
spec:
  secretRef: {name: sec1, key: api-key}
EOF
expect_eventually "B1 Credential with missing Secret -> SecretFound=false" \
  "$K get credential c1 -o jsonpath='{.status.secretFound}'" "false"

$K create secret generic sec1 --from-literal=api-key=xyz >/dev/null
expect_eventually "B2 Credential auto-converges to SecretFound=true after Secret created" \
  "$K get credential c1 -o jsonpath='{.status.secretFound}'" "true"

########################################################################
echo; echo "### Scenario C: Workflow structural validation (controller-side)"
cat <<'EOF' | $K apply -f - >/dev/null
apiVersion: core.cognet.io/v1alpha1
kind: Workflow
metadata: {name: wf-bad}
spec:
  steps:
    - {name: plan, next: [nowhere]}
EOF
expect_eventually "C1 Workflow with dangling 'next' -> Ready=False" \
  "$K get workflow wf-bad -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}'" "False"
expect_eventually "C2 reason is InvalidSpec" \
  "$K get workflow wf-bad -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].reason}'" "InvalidSpec"

cat <<'EOF' | $K apply -f - >/dev/null
apiVersion: core.cognet.io/v1alpha1
kind: Workflow
metadata: {name: wf-ok}
spec:
  steps:
    - {name: plan, next: [act]}
    - {name: act}
EOF
expect_eventually "C3 valid Workflow -> Ready=True" \
  "$K get workflow wf-ok -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}'" "True"

########################################################################
echo; echo "### Scenario D: MCPServer materializes + owns Deployment/Service, GC on delete"
cat <<'EOF' | $K apply -f - >/dev/null
apiVersion: core.cognet.io/v1alpha1
kind: MCPServer
metadata: {name: mcp1}
spec: {image: nginx:alpine, port: 8080, replicas: 1}
EOF
expect_eventually "D1 Deployment created for MCPServer" \
  "$K get deploy mcp1 -o jsonpath='{.metadata.name}'" "mcp1"
expect_eventually "D2 Service created for MCPServer" \
  "$K get svc mcp1 -o jsonpath='{.spec.ports[0].port}'" "8080"
expect_eventually "D3 Deployment owned by MCPServer (GC wiring)" \
  "$K get deploy mcp1 -o jsonpath='{.metadata.ownerReferences[0].kind}'" "MCPServer"
# nginx:alpine actually pulls, so the MCPServer should reach Ready.
expect_eventually "D4 MCPServer becomes Ready once pod is available" \
  "$K get mcpserver mcp1 -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}'" "True" 90

$K delete mcpserver mcp1 >/dev/null
expect_eventually "D5 Deployment garbage-collected after MCPServer deleted" \
  "$K get deploy mcp1 -o jsonpath='{.metadata.name}' 2>/dev/null" ""

########################################################################
echo; echo "### Scenario E: ToolPolicy conflict detection"
cat <<'EOF' | $K apply -f - >/dev/null
apiVersion: core.cognet.io/v1alpha1
kind: ToolPolicy
metadata: {name: tp-bad}
spec:
  rules:
    - {tool: refund, action: allow}
    - {tool: refund, action: deny}
EOF
expect_eventually "E1 ToolPolicy with conflicting actions -> Ready=False" \
  "$K get toolpolicy tp-bad -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}'" "False"

########################################################################
echo; echo "### Scenario F: Tool (executable capability) structural validation"
cat <<'EOF' | $K apply -f - >/dev/null
apiVersion: core.cognet.io/v1alpha1
kind: Tool
metadata: {name: tool-bad}
spec: {type: mcp}
EOF
expect_eventually "F1 mcp Tool without mcpServerRef -> Ready=False (InvalidSpec)" \
  "$K get tool tool-bad -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].reason}'" "InvalidSpec"

########################################################################
echo; echo "### Scenario G: Skill (markdown instruction pack) content source"
# inline content -> Ready, source=inline
cat <<'EOF' | $K apply -f - >/dev/null
apiVersion: core.cognet.io/v1alpha1
kind: Skill
metadata: {name: sk-inline}
spec:
  description: inline skill
  content: "# do the thing"
EOF
expect_eventually "G1 inline Skill -> Ready, contentSource=inline" \
  "$K get skill sk-inline -o jsonpath='{.status.contentSource}'" "inline"
expect_eventually "G1b inline Skill Ready=True" \
  "$K get skill sk-inline -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}'" "True"

# configMap-sourced -> Degraded until the ConfigMap exists, then Ready (watch)
cat <<'EOF' | $K apply -f - >/dev/null
apiVersion: core.cognet.io/v1alpha1
kind: Skill
metadata: {name: sk-cm}
spec:
  description: configmap skill
  contentConfigMapRef: {name: skill-body, key: SKILL.md}
EOF
expect_eventually "G2 Skill with missing ConfigMap -> Ready=False" \
  "$K get skill sk-cm -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}'" "False"
$K create configmap skill-body --from-literal=SKILL.md='# body' >/dev/null
expect_eventually "G3 Skill auto-converges to Ready after ConfigMap created (watch)" \
  "$K get skill sk-cm -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}'" "True"

########################################################################
echo; echo "### teardown"
kubectl delete ns $NS --wait=false >/dev/null 2>&1

echo
echo "==================== RESULT ===================="
echo "PASS=$PASS  FAIL=$FAIL"
[[ $FAIL -eq 0 ]] && green "ALL FUNCTIONAL TESTS PASSED" || red "SOME TESTS FAILED"
exit $FAIL