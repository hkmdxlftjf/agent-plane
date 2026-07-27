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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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

// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=agents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=agents/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=models;workflows;prompttemplates;tools;toolsets;skills;memories;policies;agentclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

// Reconcile resolves every reference declared by the Agent. If any referenced
// resource is missing, the Agent is marked not Ready with reason
// ReferenceNotFound; otherwise it computes a stable config hash over all
// resolved references and marks the Agent Ready.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var agent corev1alpha1.Agent
	if err := r.Get(ctx, req.NamespacedName, &agent); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	resolved, missing, err := r.resolveRefs(ctx, &agent)
	if err != nil {
		return ctrl.Result{}, err
	}

	agent.Status.ObservedGeneration = agent.Generation
	agent.Status.ResolvedRefs = len(resolved)

	// Materialize the runtime workload if requested (pull model).
	if agent.Spec.Runtime != nil {
		if err := r.reconcileRuntime(ctx, &agent); err != nil {
			return ctrl.Result{}, err
		}
	}

	if len(missing) > 0 {
		agent.Status.Phase = corev1alpha1.AgentPhaseDegraded
		agent.Status.ResolvedConfigHash = ""
		msg := fmt.Sprintf("unresolved references: %v", missing)
		setCondition(&agent.Status.Conditions, corev1alpha1.ConditionResolved, metav1.ConditionFalse, corev1alpha1.ReasonReferenceMissing, msg, agent.Generation)
		setCondition(&agent.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionFalse, corev1alpha1.ReasonReferenceMissing, msg, agent.Generation)
		log.Info("agent has unresolved references", "missing", missing)
		return ctrl.Result{}, r.Status().Update(ctx, &agent)
	}

	hash, err := configHash(resolved)
	if err != nil {
		return ctrl.Result{}, err
	}
	agent.Status.Phase = corev1alpha1.AgentPhaseReady
	agent.Status.ResolvedConfigHash = hash
	setCondition(&agent.Status.Conditions, corev1alpha1.ConditionResolved, metav1.ConditionTrue, corev1alpha1.ReasonReconciled, "all references resolved", agent.Generation)
	setCondition(&agent.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionTrue, corev1alpha1.ReasonReconciled, "agent configuration assembled", agent.Generation)

	return ctrl.Result{}, r.Status().Update(ctx, &agent)
}

// resolveRefs looks up each referenced object. It returns the resolved entries
// (in a stable order) and a list of "Kind/name" strings for any that are
// missing. A get error other than NotFound is returned as err.
func (r *AgentReconciler) resolveRefs(ctx context.Context, agent *corev1alpha1.Agent) (resolved []resolvedRef, missing []string, err error) {
	ns := agent.Namespace

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
	}
	if eff.WorkflowRef != nil {
		targets = append(targets, target{"Workflow", eff.WorkflowRef.Name, &corev1alpha1.Workflow{}})
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
	}
	for _, ref := range eff.PolicyRefs {
		targets = append(targets, target{"Policy", ref.Name, &corev1alpha1.Policy{}})
	}
	for _, ref := range eff.KnowledgeBaseRefs {
		targets = append(targets, target{"KnowledgeBase", ref.Name, &corev1alpha1.KnowledgeBase{}})
	}

	// An agent with no effective model (neither its own modelRef nor a class
	// default) cannot be assembled — surface it via the same Degraded path.
	if eff.ModelRef == nil {
		missing = append(missing, "Model (set spec.modelRef or an AgentClass defaultModelRef)")
	}

	for _, t := range targets {
		getErr := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: t.name}, t.obj)
		switch {
		case getErr == nil:
			resolved = append(resolved, resolvedRef{
				Kind:            t.kind,
				Name:            t.name,
				ResourceVersion: t.obj.GetResourceVersion(),
			})
		case apierrors.IsNotFound(getErr):
			missing = append(missing, fmt.Sprintf("%s/%s", t.kind, t.name))
		default:
			return nil, nil, getErr
		}
	}
	return resolved, missing, nil
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
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Agent{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Watches(&corev1alpha1.Model{}, handler.EnqueueRequestsFromMapFunc(r.agentsReferencing)).
		Watches(&corev1alpha1.Workflow{}, handler.EnqueueRequestsFromMapFunc(r.agentsReferencing)).
		Watches(&corev1alpha1.PromptTemplate{}, handler.EnqueueRequestsFromMapFunc(r.agentsReferencing)).
		Watches(&corev1alpha1.Tool{}, handler.EnqueueRequestsFromMapFunc(r.agentsReferencing)).
		Watches(&corev1alpha1.ToolSet{}, handler.EnqueueRequestsFromMapFunc(r.agentsReferencing)).
		Watches(&corev1alpha1.Skill{}, handler.EnqueueRequestsFromMapFunc(r.agentsReferencing)).
		Watches(&corev1alpha1.Memory{}, handler.EnqueueRequestsFromMapFunc(r.agentsReferencing)).
		Watches(&corev1alpha1.Policy{}, handler.EnqueueRequestsFromMapFunc(r.agentsReferencing)).
		Watches(&corev1alpha1.KnowledgeBase{}, handler.EnqueueRequestsFromMapFunc(r.agentsReferencing)).
		Watches(&corev1alpha1.AgentClass{}, handler.EnqueueRequestsFromMapFunc(r.agentsReferencing)).
		Named("agent").
		Complete(r)
}

// agentsReferencing enqueues every Agent in the changed object's namespace.
// This is intentionally coarse for the scaffold: reconciling an Agent whose
// refs did not change is cheap (a few gets + a no-op status update) and avoids
// maintaining a reverse index. A field-index optimization is a documented
// follow-up.
func (r *AgentReconciler) agentsReferencing(ctx context.Context, obj client.Object) []reconcile.Request {
	var agents corev1alpha1.AgentList
	if err := r.List(ctx, &agents, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(agents.Items))
	for i := range agents.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: agents.Items[i].Namespace,
			Name:      agents.Items[i].Name,
		}})
	}
	return reqs
}

func agentRuntimeLabels(agent *corev1alpha1.Agent) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "agent-runtime",
		"app.kubernetes.io/instance":   agent.Name,
		"app.kubernetes.io/managed-by": "agent-plane",
	}
}

// reconcileRuntime materializes the agent runtime as an owned Deployment (and
// optional Service). The runtime is a pull-model consumer: the Operator injects
// where the Registry is and which Agent to load; the container reads its config
// from the Registry (see docs/runtime-protocol.md). Agent Plane does not do inference —
// the image is user-supplied.
func (r *AgentReconciler) reconcileRuntime(ctx context.Context, agent *corev1alpha1.Agent) error {
	rt := agent.Spec.Runtime
	labels := agentRuntimeLabels(agent)
	name := agent.Name + "-runtime"
	registryURL := r.RegistryURL
	if registryURL == "" {
		registryURL = "http://agent-plane-registry.agent-plane-system.svc:9090"
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
		dep.Spec.Template.Spec.Containers = []corev1.Container{container}
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
	return nil
}
