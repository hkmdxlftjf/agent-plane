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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/cognet/agent-plane/api/v1alpha1"
)

// AgentClassReconciler reconciles an AgentClass object. It validates that the
// defaults an AgentClass hands down to Agents (model/workflow/prompt/policies)
// actually resolve, so a misconfigured class fails fast instead of silently
// producing broken Agents.
type AgentClassReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.cognet.io,resources=agentclasses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.cognet.io,resources=agentclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.cognet.io,resources=agentclasses/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.cognet.io,resources=models;workflows;prompttemplates;policies,verbs=get;list;watch

// Reconcile validates the AgentClass defaults and publishes readiness.
func (r *AgentClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var class corev1alpha1.AgentClass
	if err := r.Get(ctx, req.NamespacedName, &class); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var targets []refTarget
	if class.Spec.DefaultModelRef != nil {
		targets = append(targets, refTarget{"Model", class.Spec.DefaultModelRef.Name, &corev1alpha1.Model{}})
	}
	if class.Spec.DefaultWorkflowRef != nil {
		targets = append(targets, refTarget{"Workflow", class.Spec.DefaultWorkflowRef.Name, &corev1alpha1.Workflow{}})
	}
	if class.Spec.DefaultPromptRef != nil {
		targets = append(targets, refTarget{"PromptTemplate", class.Spec.DefaultPromptRef.Name, &corev1alpha1.PromptTemplate{}})
	}
	for _, ref := range class.Spec.DefaultPolicyRefs {
		targets = append(targets, refTarget{"Policy", ref.Name, &corev1alpha1.Policy{}})
	}

	missing, err := resolveTargets(ctx, r.Client, class.Namespace, targets)
	if err != nil {
		return ctrl.Result{}, err
	}

	class.Status.ObservedGeneration = class.Generation
	applyReadyFromMissing(&class.Status.Conditions, missing, class.Generation, "agent class ready")
	log.V(1).Info("reconciled AgentClass", "missing", missing)
	return ctrl.Result{}, r.Status().Update(ctx, &class)
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	referrers := enqueueReferrers(r.Client, func() client.ObjectList { return &corev1alpha1.AgentClassList{} })
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.AgentClass{}).
		Watches(&corev1alpha1.Model{}, referrers).
		Watches(&corev1alpha1.Workflow{}, referrers).
		Watches(&corev1alpha1.PromptTemplate{}, referrers).
		Watches(&corev1alpha1.Policy{}, referrers).
		Named("agentclass").
		Complete(r)
}
