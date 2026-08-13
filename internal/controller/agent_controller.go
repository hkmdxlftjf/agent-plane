/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

// AgentReconciler reconciles an Agent object. It resolves the agent's
// references into a single, hashable runtime configuration and publishes
// readiness so the Registry can serve that configuration to runtimes. When
// spec.runtime is set it additionally materializes an owned runtime Deployment
// (pull model — the container reads its config from the Registry).
type AgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// RegistryURL is injected into materialized runtime pods as AGENTPLANE_REGISTRY.
	RegistryURL string
}

// resolvedRef is one entry of the aggregated, order-stable config that the hash
// is computed over. Kind+Name is enough to detect ref changes; the referenced
// objects' own generations feed in via ResourceVersion.
type resolvedRef struct {
	Kind            string `json:"kind"`
	Name            string `json:"name"`
	ResourceVersion string `json:"resourceVersion"`
}

// resolution is everything one pass over an Agent's references produced: the
// hashable entries, the names of anything that did not exist, and the material
// needed to police the Agent against its Policies. Gathering the policy inputs
// here means the check costs no extra API reads.
type resolution struct {
	refs    []resolvedRef
	missing []string
	// policies and toolPolicies are the objects that resolved, in ref order.
	policies     []corev1alpha1.Policy
	toolPolicies []corev1alpha1.ToolPolicy
	// declared is what the Agent effectively points at, with ToolSets expanded
	// and each Tool's MCPServer included, so a capability reached indirectly is
	// policed the same as a directly referenced one.
	declared corev1alpha1.AgentReferences
	// skillScopes are the tool restrictions the referenced Skills declare, checked
	// against the Agent's tool surface for coherence.
	skillScopes []corev1alpha1.SkillToolScope
	// peers are the Agents this one consults through peer Tools.
	peers []peerRef
}

// peerRef is one edge in the peer graph: the Tool that declares it, and the
// Agent it points at.
type peerRef struct {
	tool  string
	agent string
}

// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=agents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=agents/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=models;workflows;prompttemplates;tools;toolsets;skills;memories;policies;toolpolicies;agentclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=credentials,verbs=get;list;watch

// Reconcile resolves every reference declared by the Agent. If any referenced
// resource is missing, the Agent is marked not Ready with reason
// ReferenceNotFound; if a referenced resource exists but a Policy forbids this
// Agent from using it, the Agent is marked not Ready with reason
// PolicyViolation. Otherwise it computes a stable config hash over all resolved
// references and marks the Agent Ready.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var agent corev1alpha1.Agent
	if err := r.Get(ctx, req.NamespacedName, &agent); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	res, err := r.resolveRefs(ctx, &agent)
	if err != nil {
		return ctrl.Result{}, err
	}

	agent.Status.ObservedGeneration = agent.Generation
	agent.Status.ResolvedRefs = len(res.refs)

	// Materialize the runtime workload if requested (pull model).
	if agent.Spec.Runtime != nil {
		if err := r.reconcileRuntime(ctx, &agent); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Publish the peer endpoint if this Agent answers other Agents.
	if agent.Spec.Expose != nil {
		if err := r.reconcilePeer(ctx, &agent); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		agent.Status.PeerEndpoint = ""
	}

	if len(res.missing) > 0 {
		agent.Status.Phase = corev1alpha1.AgentPhaseDegraded
		agent.Status.ResolvedConfigHash = ""
		msg := fmt.Sprintf("unresolved references: %v", res.missing)
		setCondition(&agent.Status.Conditions, corev1alpha1.ConditionResolved, metav1.ConditionFalse, corev1alpha1.ReasonReferenceMissing, msg, agent.Generation)
		setCondition(&agent.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionFalse, corev1alpha1.ReasonReferenceMissing, msg, agent.Generation)
		log.Info("agent has unresolved references", "missing", res.missing)
		return ctrl.Result{}, r.Status().Update(ctx, &agent)
	}

	setCondition(&agent.Status.Conditions, corev1alpha1.ConditionResolved, metav1.ConditionTrue, corev1alpha1.ReasonReconciled, "all references resolved", agent.Generation)

	// Everything the Agent points at exists; now check that it is allowed to use
	// it. Refusing here is the control plane's half of policy enforcement: an
	// Agent whose declaration is forbidden never reaches Ready, so no runtime
	// ever fetches a config for it. The call-time half (which tool this turn,
	// how many times) is enforced by the runtime.
	policy := corev1alpha1.MergePolicies(res.policies, res.toolPolicies)
	violations := policy.Violations(res.declared)
	// A Skill that allows a tool the Agent cannot reach is broken on its face: its
	// instructions would send the model after something uncallable. Reported
	// alongside policy violations because the operator's fix is the same shape —
	// edit a declaration — and both mean "exists, but incoherent".
	violations = append(violations, corev1alpha1.SkillToolViolations(res.skillScopes, res.declared.Tools)...)
	// A peer Tool pointing at itself, or at an Agent that publishes no endpoint,
	// resolves to nothing — the calling model would advertise a tool whose every
	// call fails. Same class of error as an unreachable Skill tool, so it lands
	// in the same report.
	violations = append(violations, r.peerViolations(ctx, &agent, res.peers)...)
	if len(violations) > 0 {
		agent.Status.Phase = corev1alpha1.AgentPhaseDegraded
		agent.Status.ResolvedConfigHash = ""
		msg := violationMessage(violations, policy)
		setCondition(&agent.Status.Conditions, corev1alpha1.ConditionPolicyCompliant, metav1.ConditionFalse, corev1alpha1.ReasonPolicyViolation, msg, agent.Generation)
		setCondition(&agent.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionFalse, corev1alpha1.ReasonPolicyViolation, msg, agent.Generation)
		log.Info("agent declaration is not permitted", "violations", violations, "policySources", policy.PolicySources())
		return ctrl.Result{}, r.Status().Update(ctx, &agent)
	}
	setCondition(&agent.Status.Conditions, corev1alpha1.ConditionPolicyCompliant, metav1.ConditionTrue, corev1alpha1.ReasonReconciled, policyCompliantMessage(policy), agent.Generation)

	hash, err := configHash(res.refs)
	if err != nil {
		return ctrl.Result{}, err
	}
	agent.Status.Phase = corev1alpha1.AgentPhaseReady
	agent.Status.ResolvedConfigHash = hash
	setCondition(&agent.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionTrue, corev1alpha1.ReasonReconciled, "agent configuration assembled", agent.Generation)

	return ctrl.Result{}, r.Status().Update(ctx, &agent)
}

// violationMessage renders the refusal for a status condition. The Policy
// sources are only cited when policies are actually in play — a Skill/tool
// coherence failure on an Agent with no Policy would otherwise read as
// "violations … (from [])".
func violationMessage(violations []string, p *corev1alpha1.EffectivePolicy) string {
	if p == nil || len(p.Sources) == 0 {
		return fmt.Sprintf("declaration not permitted: %v", violations)
	}
	return fmt.Sprintf("declaration not permitted: %v (policies: %v)", violations, p.Sources)
}

// policyCompliantMessage distinguishes "checked against policies and passed"
// from "no policies apply", so a green condition is not mistaken for enforcement
// that is not actually configured.
func policyCompliantMessage(p *corev1alpha1.EffectivePolicy) string {
	if p == nil {
		return "no policies referenced"
	}
	return fmt.Sprintf("references permitted by %v", p.Sources)
}

// resolveRefs looks up each referenced object. It returns the resolved entries
// (in a stable order), a list of "Kind/name" strings for any that are missing,
// and the policy material gathered along the way. A get error other than
// NotFound is returned as err.
func (r *AgentReconciler) resolveRefs(ctx context.Context, agent *corev1alpha1.Agent) (*resolution, error) {
	ns := agent.Namespace
	res := &resolution{}

	// (Kind, name, object) tuples to resolve. Order here defines hash order.
	type target struct {
		kind string
		name string
		obj  client.Object
	}
	targets := make([]target, 0, 12)

	// Resolve the AgentClass first so its defaults can fill unset Agent refs.
	// The class itself is also a resolved reference (so its changes bump the hash).
	var class *corev1alpha1.AgentClass
	if agent.Spec.AgentClassRef != nil {
		targets = append(targets, target{"AgentClass", agent.Spec.AgentClassRef.Name, &corev1alpha1.AgentClass{}})
		var c corev1alpha1.AgentClass
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: agent.Spec.AgentClassRef.Name}, &c); err == nil {
			class = &c
		}
	}
	eff := corev1alpha1.ApplyClassDefaults(agent.Spec, class)

	if eff.ModelRef != nil {
		targets = append(targets, target{kindModel, eff.ModelRef.Name, &corev1alpha1.Model{}})
		res.declared.Model = eff.ModelRef.Name
	}
	if eff.WorkflowRef != nil {
		targets = append(targets, target{"Workflow", eff.WorkflowRef.Name, &corev1alpha1.Workflow{}})
		res.declared.Workflow = eff.WorkflowRef.Name
	}
	if eff.PromptRef != nil {
		targets = append(targets, target{"PromptTemplate", eff.PromptRef.Name, &corev1alpha1.PromptTemplate{}})
	}
	for _, ref := range eff.ToolRefs {
		targets = append(targets, target{kindTool, ref.Name, &corev1alpha1.Tool{}})
	}
	for _, ref := range eff.ToolSetRefs {
		targets = append(targets, target{"ToolSet", ref.Name, &corev1alpha1.ToolSet{}})
	}
	for _, ref := range eff.SkillRefs {
		targets = append(targets, target{"Skill", ref.Name, &corev1alpha1.Skill{}})
	}
	for _, ref := range eff.MemoryRefs {
		targets = append(targets, target{kindMemory, ref.Name, &corev1alpha1.Memory{}})
		res.declared.Memories = append(res.declared.Memories, ref.Name)
	}
	for _, ref := range eff.PolicyRefs {
		targets = append(targets, target{"Policy", ref.Name, &corev1alpha1.Policy{}})
	}
	for _, ref := range eff.ToolPolicyRefs {
		targets = append(targets, target{"ToolPolicy", ref.Name, &corev1alpha1.ToolPolicy{}})
	}
	for _, ref := range eff.KnowledgeBaseRefs {
		targets = append(targets, target{"KnowledgeBase", ref.Name, &corev1alpha1.KnowledgeBase{}})
	}

	// An agent with no effective model (neither its own modelRef nor a class
	// default) cannot be assembled — surface it via the same Degraded path.
	if eff.ModelRef == nil {
		res.missing = append(res.missing, "Model (set spec.modelRef or an AgentClass defaultModelRef)")
	}

	for _, t := range targets {
		getErr := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: t.name}, t.obj)
		switch {
		case getErr == nil:
			res.refs = append(res.refs, resolvedRef{
				Kind:            t.kind,
				Name:            t.name,
				ResourceVersion: t.obj.GetResourceVersion(),
			})
			// Collect the policy inputs while the objects are in hand.
			switch obj := t.obj.(type) {
			case *corev1alpha1.Policy:
				res.policies = append(res.policies, *obj)
			case *corev1alpha1.ToolPolicy:
				res.toolPolicies = append(res.toolPolicies, *obj)
			case *corev1alpha1.Skill:
				if len(obj.Spec.AllowedTools) > 0 {
					res.skillScopes = append(res.skillScopes, corev1alpha1.SkillToolScope{
						Skill:        obj.Name,
						AllowedTools: obj.Spec.AllowedTools,
					})
				}
			}
		case apierrors.IsNotFound(getErr):
			res.missing = append(res.missing, fmt.Sprintf("%s/%s", t.kind, t.name))
		default:
			return nil, getErr
		}
	}

	// Expand the tool surface the Agent actually gets: direct toolRefs plus every
	// Tool a referenced ToolSet contributes, plus the MCPServer behind each. A
	// policy denying an MCPServer must bite even when the tool arrives via a set.
	toolNames := make([]string, 0, len(eff.ToolRefs))
	for _, ref := range eff.ToolRefs {
		toolNames = append(toolNames, ref.Name)
	}
	for _, ref := range eff.ToolSetRefs {
		var ts corev1alpha1.ToolSet
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &ts); err != nil {
			continue // missing ToolSets are already reported above
		}
		for _, tr := range ts.Spec.ToolRefs {
			toolNames = append(toolNames, tr.Name)
		}
	}
	res.declared.Tools = dedupe(toolNames)
	var mcpNames []string
	for _, name := range res.declared.Tools {
		var tool corev1alpha1.Tool
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &tool); err != nil {
			continue
		}
		if tool.Spec.MCPServerRef != nil {
			mcpNames = append(mcpNames, tool.Spec.MCPServerRef.Name)
		}
		// A peer Tool names another Agent. Record it so the coherence check below
		// can verify the target actually answers, and so the peer graph is visible
		// in one place.
		if tool.Spec.AgentRef != nil {
			res.peers = append(res.peers, peerRef{tool: name, agent: tool.Spec.AgentRef.Name})
		}
	}
	res.declared.MCPServers = dedupe(mcpNames)

	return res, nil
}

// dedupe returns names with duplicates removed, order preserved.
func dedupe(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// configHash produces a deterministic hash over the resolved references. The
// input order is fixed by resolveRefs, so the hash changes only when a
// reference is added/removed or a referenced object changes.
func configHash(resolved []resolvedRef) (string, error) {
	b, err := json.Marshal(resolved)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// setCondition upserts a status condition with ObservedGeneration set.
func setCondition(conds *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, msg string, generation int64) {
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: generation,
	})
}

// SetupWithManager wires the controller. In addition to watching Agents, it
// watches the referenceable kinds so that a change to a Model/Tool/etc.
// re-reconciles the Agents that reference it (the "recompute runtime config on
// dependency change" behavior from the lifecycle design).
//
// Each watch is reference-precise: a field index maps the changed dependency
// back to just the Agents that use it. Two routes reach an Agent without it
// naming the dependency directly, and both are followed explicitly —
// inheritance from an AgentClass default, and expansion of a ToolSet into its
// member Tools. Missing either would silently drop events, which is worse than
// the namespace-wide fan-out this replaces.
//
// SetupFieldIndexes must have run on this manager first.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Agent{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		// Model/Workflow/Prompt/Policy can be inherited from an AgentClass, so each
		// also consults the matching class index.
		Watches(&corev1alpha1.Model{},
			r.agentsReferencingIndexed([]string{idxAgentModel}, []string{idxClassModel}, false)).
		Watches(&corev1alpha1.Workflow{},
			r.agentsReferencingIndexed([]string{idxAgentWorkflow}, []string{idxClassWorkflow}, false)).
		Watches(&corev1alpha1.PromptTemplate{},
			r.agentsReferencingIndexed([]string{idxAgentPrompt}, []string{idxClassPrompt}, false)).
		Watches(&corev1alpha1.Policy{},
			r.agentsReferencingIndexed([]string{idxAgentPolicies}, []string{idxClassPolicies}, false)).
		// A Tool reaches an Agent directly or through a ToolSet.
		Watches(&corev1alpha1.Tool{},
			r.agentsReferencingIndexed([]string{idxAgentTools}, nil, true)).
		Watches(&corev1alpha1.ToolSet{},
			r.agentsReferencingIndexed([]string{idxAgentToolSets}, nil, false)).
		// The rest are only ever referenced directly.
		Watches(&corev1alpha1.Skill{},
			r.agentsReferencingIndexed([]string{idxAgentSkills}, nil, false)).
		Watches(&corev1alpha1.Memory{},
			r.agentsReferencingIndexed([]string{idxAgentMemories}, nil, false)).
		Watches(&corev1alpha1.ToolPolicy{},
			r.agentsReferencingIndexed([]string{idxAgentToolPolicies}, nil, false)).
		Watches(&corev1alpha1.KnowledgeBase{},
			r.agentsReferencingIndexed([]string{idxAgentKnowledge}, nil, false)).
		Watches(&corev1alpha1.AgentClass{},
			r.agentsReferencingIndexed([]string{idxAgentClass}, nil, false)).
		Named("agent").
		Complete(r)
}

// workspaceVolumeName is the volume carrying the Agent's working tree.
const workspaceVolumeName = "workspace"

// gitCredentialMountPath is where a workspace Credential's Secret is mounted in
// the clone step. As elsewhere, the value is mounted rather than passed as
// environment so it stays out of `kubectl describe pod`.
const gitCredentialMountPath = "/var/run/agentplane/git"

// defaultWorkspaceMountPath is where the working tree appears when the Agent
// does not say. Matches the CRD default.
const defaultWorkspaceMountPath = "/workspace"

// gitCredentialHelper is the same shell-based credential helper the clone init
// container installs, reused verbatim so the runtime container can push with
// the identical token: it reads the mounted Secret file at call time rather
// than embedding the token in the remote URL or the environment.
const gitCredentialHelper = `!f() { echo "username=x-access-token"; echo "password=$(cat "$AGENTPLANE_GIT_CREDENTIAL_FILE")"; }; f`

// modelCredentialMountPath is where a workspace runtime finds its model
// credential as files, one per Secret key.
//
// The Registry serves Secret *coordinates* and a runtime normally reads the
// Secret itself through the Kubernetes API (docs/runtime-protocol.md §1). A
// runtime that is not written in Go — opencode plus a plugin, say — would need
// a Kubernetes client to do that, and would then hold an API-server credential
// in the same process that runs model-directed shell commands. Mounting the
// Secret read-only instead keeps that credential out of the pod entirely; the
// coordinate that named the key in the payload also locates it on disk.
const modelCredentialMountPath = "/var/run/agentplane/model"

// agentHomeDirName is the writable HOME a workspace runtime gets, as a
// subdirectory of the working tree's volume.
//
// A workspace pod's root filesystem is read-only (see the SecurityContext
// below), but a coding-agent runtime needs somewhere durable to keep its own
// state — session history, caches, and (for opencode) the plugin dependencies
// it resolves at startup. Putting HOME on the same PersistentVolumeClaim as the
// checkout means that state survives a restart exactly as the branches do,
// which is what makes a conversation resumable after the pod is rescheduled.
// /tmp would be writable too, but it is an emptyDir: every restart would start
// the conversation over.
const agentHomeDirName = ".agent-home"

// defaultWorkspaceReadinessProbe is what "ready" means for a workspace-bound
// runtime when the Agent does not say.
//
// It deliberately does not probe a health endpoint. A coding-agent runtime does
// real work before it can answer — fetches its config, resolves plugins, opens an
// upstream connection — and while that is in flight it already listens on its
// port and already answers a trivial health check. Every startup failure this
// repository has hit looked identical from the outside: 1/1 Running, health 200,
// and every request hanging forever. A probe that cannot tell those apart reports
// the pod as ready and hides the failure.
//
// So the probe asks for the config the runtime is supposed to have *projected*,
// and requires the Agent Plane provider to be in it. That string is only present
// once startup got far enough to matter. failureThreshold is generous because the
// first start on a cold volume legitimately takes minutes (a model catalog can be
// megabytes); a slow start should not be reported as a broken one.
func defaultWorkspaceReadinessProbe(port int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{
					"sh", "-c",
					fmt.Sprintf("curl -sf -m 5 http://127.0.0.1:%d/config | grep -q '%s'", port, projectedModelMarker),
				},
			},
		},
		InitialDelaySeconds: 20,
		PeriodSeconds:       15,
		TimeoutSeconds:      10,
		FailureThreshold:    12,
	}
}

// projectedModelMarker is the substring a *projected* runtime config contains,
// and an unprojected one does not.
//
// Matching the bare provider id would be wrong, and wrongly reassuring: the
// string "agent-plane" also appears in the plugin's own file path, which the
// entrypoint writes into the config before opencode has done anything — so that
// looser check reports ready on precisely the boot where projection failed.
// Anchoring on the selected model instead means the probe only passes once the
// runtime has resolved a Model through the Registry and pointed itself at it.
const projectedModelMarker = `"model":"agent-plane/`

func boolPtr(b bool) *bool { return &b }

func int64Ptr(i int64) *int64 { return &i }

// workspaceFSGroup is the group every workspace-bound pod's containers share.
// The clone init container runs as root and the runtime container drops to
// whatever non-root uid its image picks — a uid the Operator cannot see (see
// the safe.directory comment below). Setting fsGroup instead of trying to
// match uids: kubelet chowns the volume to this group and sets the setgid bit
// on its directories, so files git creates as root are still group-writable,
// and every container gets this group as a supplementary group regardless of
// its own uid. That is what makes a coding agent's write_file tool and git's
// own writes (commits, config) work without knowing the runtime image's uid.
const workspaceFSGroup = int64(10001)

func agentRuntimeLabels(agent *corev1alpha1.Agent) map[string]string {
	return ownedLabels("agent-runtime", agent.Name)
}

func workspaceClaimName(agent *corev1alpha1.Agent) string {
	return agent.Name + "-workspace"
}

// reconcileWorkspace provisions the PersistentVolumeClaim holding the Agent's
// working tree.
//
// A PVC's storage request is immutable in most clusters, so the claim is created
// once and thereafter left alone: shrinking is never allowed, and growing needs
// an expansion-capable StorageClass. Rewriting the spec on every reconcile would
// fail the update against an unchanged claim.
func (r *AgentReconciler) reconcileWorkspace(ctx context.Context, agent *corev1alpha1.Agent) error {
	ws := agent.Spec.Workspace
	name := workspaceClaimName(agent)

	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: name}, &existing)
	switch {
	case err == nil:
		return nil // already provisioned; size and class are immutable
	case !apierrors.IsNotFound(err):
		return err
	}

	size := ws.Size
	if size == "" {
		size = "10Gi"
	}
	quantity, parseErr := resource.ParseQuantity(size)
	if parseErr != nil {
		return fmt.Errorf("workspace size %q: %w", size, parseErr)
	}

	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: agent.Namespace,
			Labels:    agentRuntimeLabels(agent),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			// One Agent owns one working tree, and the Deployment is pinned to a
			// single replica for the same reason — ReadWriteOnce matches that and
			// works on every storage backend.
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ws.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: quantity},
			},
		},
	}
	if err := controllerutil.SetControllerReference(agent, claim, r.Scheme); err != nil {
		return err
	}
	return client.IgnoreAlreadyExists(r.Create(ctx, claim))
}

// workspaceMountPath returns where the working tree appears in the container.
func workspaceMountPath(ws *corev1alpha1.AgentWorkspaceSpec) string {
	if ws.MountPath != "" {
		return ws.MountPath
	}
	return defaultWorkspaceMountPath
}

// cloneInitContainer builds the step that populates the working tree before the
// runtime starts.
//
// It is idempotent by design: an existing clone is fetched and reset rather than
// re-cloned, so a pod restart resumes on the same tree instead of discarding
// work. The credential is mounted read-only and consumed by a credential helper,
// never interpolated into the remote URL — a URL-embedded token leaks into
// `git remote -v`, the reflog, and any error message git prints.
func cloneInitContainer(agent *corev1alpha1.Agent, hasCredential bool) corev1.Container {
	ws := agent.Spec.Workspace
	mount := workspaceMountPath(ws)

	script := `set -eu
REPO="$AGENTPLANE_REPOSITORY"
DIR="$AGENTPLANE_WORKSPACE"
if [ -n "${AGENTPLANE_GIT_CREDENTIAL_FILE:-}" ] && [ -f "$AGENTPLANE_GIT_CREDENTIAL_FILE" ]; then
  # Feed the token to git without putting it in the URL or the environment.
  git config --global credential.helper \
    '!f() { echo "username=x-access-token"; echo "password=$(cat '"'"'$AGENTPLANE_GIT_CREDENTIAL_FILE'"'"')"; }; f'
fi
if [ -d "$DIR/.git" ]; then
  echo "workspace already cloned; fetching"
  git -C "$DIR" remote set-url origin "$REPO"
  git -C "$DIR" fetch --prune origin
  if [ -n "${AGENTPLANE_BRANCH:-}" ]; then
    git -C "$DIR" checkout "$AGENTPLANE_BRANCH"
    git -C "$DIR" reset --hard "origin/$AGENTPLANE_BRANCH"
  fi
else
  echo "cloning $REPO"
  if [ -n "${AGENTPLANE_BRANCH:-}" ]; then
    git clone --branch "$AGENTPLANE_BRANCH" "$REPO" "$DIR"
  else
    git clone "$REPO" "$DIR"
  fi
fi
# The runtime's HOME lives on this volume (see agentHomeDirName): create it here,
# while we are still root, and tell git to ignore it. Without the exclude a
# coding agent would see its own state directory as untracked files in the
# repository it is working on — and could commit them.
#
# chmod is not optional. This step runs as root, so the directory is created
# 0755 root-owned, and the runtime container runs as a non-root uid whose only
# shared credential is the fsGroup — it cannot write into it and dies on
# startup. The failure only appears on a *fresh* volume, which is why it is easy
# to miss: an existing workspace already has a usable directory.
#
# The /** suffix matches the directory's *contents*, which is what survives a
# repository whose own .gitignore re-includes directories (the "/*" then "!/*/"
# idiom): such a negation beats a plain directory pattern in info/exclude, and
# the agent's logs reappear as untracked files no matter what is written here.
mkdir -p "$DIR/` + agentHomeDirName + `"
chmod 2775 "$DIR/` + agentHomeDirName + `"
if [ -d "$DIR/.git" ]; then
  mkdir -p "$DIR/.git/info"
  # All of the runtime's own directories, not just HOME: this step runs as root
  # and the runtime may not be able to write .git/info itself.
  for p in '/` + agentHomeDirName + `/**' '/.opencode/**' '/node_modules/**'; do
    grep -qxF "$p" "$DIR/.git/info/exclude" 2>/dev/null \
      || echo "$p" >> "$DIR/.git/info/exclude"
  done
fi`

	env := []corev1.EnvVar{
		{Name: "AGENTPLANE_REPOSITORY", Value: ws.Repository},
		{Name: "AGENTPLANE_WORKSPACE", Value: mount},
	}
	if ws.Branch != "" {
		env = append(env, corev1.EnvVar{Name: "AGENTPLANE_BRANCH", Value: ws.Branch})
	}
	mounts := []corev1.VolumeMount{{Name: workspaceVolumeName, MountPath: mount}}
	if hasCredential {
		env = append(env, corev1.EnvVar{
			Name:  "AGENTPLANE_GIT_CREDENTIAL_FILE",
			Value: gitCredentialMountPath + "/token",
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "git-credential",
			MountPath: gitCredentialMountPath,
			ReadOnly:  true,
		})
	}

	return corev1.Container{
		Name:         "clone",
		Image:        "alpine/git:latest",
		Command:      []string{"/bin/sh", "-c", script},
		Env:          env,
		VolumeMounts: mounts,
	}
}

// reconcileRuntime materializes the agent runtime as an owned Deployment (and
// optional Service). The runtime is a pull-model consumer: the Operator injects
// where the Registry is and which Agent to load; the container reads its config
// from the Registry (see docs/runtime-protocol.md). Agent Plane does not do inference —
// the image is user-supplied.
//
// When spec.workspace is set the pod additionally gets the working tree: a
// clone init container populates a PersistentVolumeClaim, and the runtime
// container mounts it. That is what lets a coding agent keep a checkout, its
// branches, and its build cache across restarts.
func (r *AgentReconciler) reconcileRuntime(ctx context.Context, agent *corev1alpha1.Agent) error {
	rt := agent.Spec.Runtime
	ws := agent.Spec.Workspace
	labels := agentRuntimeLabels(agent)
	name := agent.Name + "-runtime"
	registryURL := r.RegistryURL
	if registryURL == "" {
		registryURL = "http://agent-plane-registry.agent-plane-system.svc:9090"
	}

	// Resolve the git credential to a Secret name before touching the Deployment,
	// so a missing Credential surfaces as a reconcile error rather than a pod
	// that crash-loops on an absent volume.
	gitSecret := ""
	if ws != nil && ws.CredentialRef != nil {
		var cred corev1alpha1.Credential
		if err := r.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: ws.CredentialRef.Name}, &cred); err != nil {
			return fmt.Errorf("resolve workspace credential %q: %w", ws.CredentialRef.Name, err)
		}
		gitSecret = cred.Spec.SecretRef.Name
	}

	// Resolve the model credential the same way, for workspace runtimes that
	// read it from a file rather than through the Kubernetes API (see
	// modelCredentialMountPath). Best-effort on purpose: an Agent whose Model or
	// Credential is not resolvable yet is already reported as not Ready by the
	// dependency check, and failing the whole reconcile here would keep the
	// Deployment from existing at all — the runtime is expected to start, log
	// that the credential is missing, and converge when it appears.
	modelSecret := ""
	if ws != nil {
		modelSecret = r.resolveModelSecret(ctx, agent)
	}

	if ws != nil {
		if err := r.reconcileWorkspace(ctx, agent); err != nil {
			return err
		}
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agent.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		replicas := rt.Replicas
		dep.Labels = labels
		dep.Spec.Replicas = &replicas
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		dep.Spec.Template.Labels = labels
		env := append([]corev1.EnvVar{
			{Name: "AGENTPLANE_REGISTRY", Value: registryURL},
			{Name: "AGENTPLANE_AGENT_NAMESPACE", Value: agent.Namespace},
			{Name: "AGENTPLANE_AGENT_NAME", Value: agent.Name},
		}, rt.Env...)
		container := corev1.Container{
			Name:      "runtime",
			Image:     rt.Image,
			Env:       env,
			Resources: rt.Resources,
		}
		if rt.Port != 0 {
			container.Ports = []corev1.ContainerPort{{Name: portNameHTTP, ContainerPort: rt.Port}}
		}
		// An explicit probe wins; a workspace runtime with a port gets the default
		// (see defaultWorkspaceReadinessProbe). Everything else stays unprobed:
		// for a runtime that only serves /api/chat, listening really is ready.
		switch {
		case rt.ReadinessProbe != nil:
			container.ReadinessProbe = rt.ReadinessProbe
		case ws != nil && rt.Port != 0:
			container.ReadinessProbe = defaultWorkspaceReadinessProbe(rt.Port)
		}
		if ws == nil {
			// Clear anything left behind by a workspace that was removed.
			dep.Spec.Strategy = appsv1.DeploymentStrategy{}
			dep.Spec.Template.Spec.InitContainers = nil
			dep.Spec.Template.Spec.Volumes = nil
			dep.Spec.Template.Spec.SecurityContext = nil
		} else {
			mount := workspaceMountPath(ws)
			// Declaring the tree safe through git's environment-based config avoids
			// having to know the runtime image's uid, which the Operator cannot see
			// (the clone init container runs as root; the runtime image typically
			// drops to a non-root uid, and git refuses to operate on a tree owned by
			// a different user — "detected dubious ownership"). Writing ~/.gitconfig
			// would need that uid's home directory to exist and be writable; this
			// does not.
			gitConfigCount := 1
			gitConfigEnv := []corev1.EnvVar{
				{Name: "GIT_CONFIG_KEY_0", Value: "safe.directory"},
				{Name: "GIT_CONFIG_VALUE_0", Value: mount},
			}
			mounts := []corev1.VolumeMount{
				{Name: workspaceVolumeName, MountPath: mount},
				// A coding agent's bash/file tools are the actual sandbox boundary: the
				// container's SecurityContext below locks the root filesystem read-only,
				// so anything a build or editor writes outside the working tree (caches,
				// temp files) needs somewhere to land.
				{Name: "tmp", MountPath: "/tmp"},
			}
			if gitSecret != "" {
				// The runtime container needs the same push credential the clone step
				// used to read the repository — otherwise a coding agent can read but
				// never push its own commits. Mounted read-only and consumed through a
				// credential helper, never interpolated into the remote URL.
				gitConfigCount = 2
				gitConfigEnv = append(gitConfigEnv,
					corev1.EnvVar{Name: "GIT_CONFIG_KEY_1", Value: "credential.helper"},
					corev1.EnvVar{Name: "GIT_CONFIG_VALUE_1", Value: gitCredentialHelper},
				)
				mounts = append(mounts, corev1.VolumeMount{
					Name:      "git-credential",
					MountPath: gitCredentialMountPath,
					ReadOnly:  true,
				})
			}
			if modelSecret != "" {
				mounts = append(mounts, corev1.VolumeMount{
					Name:      "model-credential",
					MountPath: modelCredentialMountPath,
					ReadOnly:  true,
				})
			}
			// HOME must be writable and durable: the root filesystem is read-only,
			// and a runtime that keeps conversation state (or resolves plugin
			// dependencies) at startup needs that to survive a restart. XDG_* are
			// set alongside HOME because tools split state across them and only
			// some derive from HOME.
			home := mount + "/" + agentHomeDirName
			container.Env = append(container.Env,
				corev1.EnvVar{Name: "AGENTPLANE_WORKSPACE", Value: mount},
				corev1.EnvVar{Name: "AGENTPLANE_AGENT_HOME", Value: home},
				corev1.EnvVar{Name: "HOME", Value: home},
				corev1.EnvVar{Name: "XDG_CONFIG_HOME", Value: home + "/.config"},
				corev1.EnvVar{Name: "XDG_DATA_HOME", Value: home + "/share"},
				corev1.EnvVar{Name: "XDG_STATE_HOME", Value: home + "/state"},
				corev1.EnvVar{Name: "XDG_CACHE_HOME", Value: home + "/cache"},
				corev1.EnvVar{Name: "AGENTPLANE_GIT_CREDENTIAL_FILE", Value: gitCredentialMountPath + "/token"},
				corev1.EnvVar{Name: "GIT_CONFIG_COUNT", Value: strconv.Itoa(gitConfigCount)},
			)
			container.Env = append(container.Env, gitConfigEnv...)
			container.VolumeMounts = mounts

			// A workspace-bound runtime executes model-directed shell commands against
			// a real checkout, so it gets the same isolation a build sandbox would:
			// no root, no added Linux capabilities, no privilege escalation, the
			// default seccomp profile, and a read-only root filesystem — the working
			// tree and /tmp are the only writable paths. This is enforced by the
			// container runtime, not by the agent's own tool code, so it holds even
			// if a tool implementation has a bug.
			container.SecurityContext = &corev1.SecurityContext{
				AllowPrivilegeEscalation: boolPtr(false),
				ReadOnlyRootFilesystem:   boolPtr(true),
				RunAsNonRoot:             boolPtr(true),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			}

			// A working tree has exactly one writer. RollingUpdate would start the
			// new pod before the old one exits, so two agents would edit the same
			// checkout — and with ReadWriteOnce the new pod would simply fail to
			// schedule. Recreate at one replica is the only correct combination.
			one := int32(1)
			dep.Spec.Replicas = &one
			dep.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}

			volumes := []corev1.Volume{
				{
					Name: workspaceVolumeName,
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: workspaceClaimName(agent),
						},
					},
				},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			}
			if gitSecret != "" {
				volumes = append(volumes, corev1.Volume{
					Name: "git-credential",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: gitSecret},
					},
				})
			}
			if modelSecret != "" {
				volumes = append(volumes, corev1.Volume{
					Name: "model-credential",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: modelSecret},
					},
				})
			}
			dep.Spec.Template.Spec.Volumes = volumes
			dep.Spec.Template.Spec.InitContainers = []corev1.Container{
				cloneInitContainer(agent, gitSecret != ""),
			}
			dep.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{
				FSGroup: int64Ptr(workspaceFSGroup),
			}
		}

		dep.Spec.Template.Spec.Containers = []corev1.Container{container}
		dep.Spec.Template.Spec.ServiceAccountName = rt.ServiceAccountName
		// Assigned unconditionally, including the empty case: clearing the field is
		// how removing spec.runtime.runtimeClassName takes effect, since this is a
		// CreateOrUpdate over an existing Deployment.
		dep.Spec.Template.Spec.RuntimeClassName = nil
		if rt.RuntimeClassName != "" {
			runtimeClass := rt.RuntimeClassName
			dep.Spec.Template.Spec.RuntimeClassName = &runtimeClass
		}
		return controllerutil.SetControllerReference(agent, dep, r.Scheme)
	}); err != nil {
		return err
	}

	if rt.Port != 0 {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agent.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
			svc.Labels = labels
			svc.Spec.Selector = labels
			svc.Spec.Ports = []corev1.ServicePort{{
				Name: portNameHTTP, Port: rt.Port, TargetPort: intstr.FromInt32(rt.Port), Protocol: corev1.ProtocolTCP,
			}}
			return controllerutil.SetControllerReference(agent, svc, r.Scheme)
		}); err != nil {
			return err
		}
	}

	// Reflect availability into status (best-effort).
	var live appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: name}, &live); err == nil {
		agent.Status.RuntimeAvailableReplicas = live.Status.AvailableReplicas
	}
	if ws != nil {
		agent.Status.WorkspaceClaim = workspaceClaimName(agent)
	} else {
		agent.Status.WorkspaceClaim = ""
	}
	return nil
}

// resolveModelSecret returns the Secret name holding the Agent's model
// credential, or "" when it cannot be resolved yet.
//
// It walks the same path the Registry does (Agent → effective spec → Model →
// Credential → Secret), so a runtime that reads the credential from a file sees
// exactly the Secret whose coordinates the Registry reported. Every step is
// best-effort: this only decides whether to add a volume, and an Agent missing
// its Model is already surfaced as not Ready by the dependency check.
func (r *AgentReconciler) resolveModelSecret(ctx context.Context, agent *corev1alpha1.Agent) string {
	var class *corev1alpha1.AgentClass
	if ref := agent.Spec.AgentClassRef; ref != nil {
		var c corev1alpha1.AgentClass
		if err := r.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: ref.Name}, &c); err == nil {
			class = &c
		}
	}
	eff := corev1alpha1.ApplyClassDefaults(agent.Spec, class)
	if eff.ModelRef == nil {
		return ""
	}
	var model corev1alpha1.Model
	if err := r.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: eff.ModelRef.Name}, &model); err != nil {
		return ""
	}
	if model.Spec.CredentialRef == nil {
		return ""
	}
	var cred corev1alpha1.Credential
	if err := r.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: model.Spec.CredentialRef.Name}, &cred); err != nil {
		return ""
	}
	return cred.Spec.SecretRef.Name
}

// peerServiceName is the Service other Agents dial to consult this one.
func peerServiceName(agent *corev1alpha1.Agent) string {
	return agent.Name + "-peer"
}

// peerViolations reports peer Tools that cannot resolve to a working endpoint.
//
// Deliberately *not* reported: a peer that exists and is exposed but is
// currently Degraded or has no ready replicas. That is a transient runtime
// condition — failing the caller for it would make one broken repository's agent
// cascade into every agent that consults it.
func (r *AgentReconciler) peerViolations(ctx context.Context, agent *corev1alpha1.Agent, peers []peerRef) []string {
	var out []string
	for _, p := range peers {
		if p.agent == agent.Name {
			out = append(out, fmt.Sprintf("tool %q points at this Agent itself; an Agent cannot consult itself", p.tool))
			continue
		}
		var target corev1alpha1.Agent
		switch err := r.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: p.agent}, &target); {
		case apierrors.IsNotFound(err):
			out = append(out, fmt.Sprintf("tool %q references Agent %q, which does not exist", p.tool, p.agent))
			continue
		case err != nil:
			continue // transient; the next reconcile retries
		}
		if target.Spec.Expose == nil {
			out = append(out, fmt.Sprintf(
				"tool %q references Agent %q, which does not set spec.expose and so answers no peers", p.tool, p.agent))
		}
	}
	sort.Strings(out)
	return out
}

// PeerEndpointFor returns the in-cluster MCP address of an exposed Agent, or ""
// when the Agent does not expose one. Exported because the Registry resolves
// peer Tools with the same rule the Operator publishes — keeping one definition
// stops the two from disagreeing about where a peer lives.
func PeerEndpointFor(agent *corev1alpha1.Agent) string {
	if agent.Spec.Expose == nil {
		return ""
	}
	port := agent.Spec.Expose.Port
	if port == 0 {
		port = 8080
	}
	return fmt.Sprintf("http://%s-peer.%s.svc:%d", agent.Name, agent.Namespace, port)
}

// reconcilePeer publishes an exposed Agent's runtime under its own Service, so
// other Agents reach it at a stable address that does not change if the runtime
// Service's port is later repurposed.
//
// No new workload is created — the Service selects the same runtime pods. What
// this adds is an addressable identity for the Agent *as a peer*, which is what
// a referencing Tool resolves to.
func (r *AgentReconciler) reconcilePeer(ctx context.Context, agent *corev1alpha1.Agent) error {
	port := agent.Spec.Expose.Port
	if port == 0 {
		port = 8080
	}
	labels := agentRuntimeLabels(agent)

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: peerServiceName(agent), Namespace: agent.Namespace,
	}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Selector = labels
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       portNameMCP,
			Port:       port,
			TargetPort: intstr.FromInt32(port),
			Protocol:   corev1.ProtocolTCP,
		}}
		return controllerutil.SetControllerReference(agent, svc, r.Scheme)
	}); err != nil {
		return err
	}

	agent.Status.PeerEndpoint = PeerEndpointFor(agent)
	return nil
}
