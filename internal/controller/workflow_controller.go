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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

// WorkflowReconciler reconciles a Workflow object. Agent Plane does not execute the
// workflow graph, but it validates the declaration is internally consistent:
// step names are unique and every `next` target points at a declared step.
type WorkflowReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=workflows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=workflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=workflows/finalizers,verbs=update

// Reconcile validates the Workflow graph and publishes readiness.
func (r *WorkflowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var wf corev1alpha1.Workflow
	if err := r.Get(ctx, req.NamespacedName, &wf); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	wf.Status.ObservedGeneration = wf.Generation

	if err := wf.Spec.Validate(); err != nil {
		setCondition(&wf.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionFalse, corev1alpha1.ReasonInvalidSpec, err.Error(), wf.Generation)
	} else {
		setCondition(&wf.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionTrue, corev1alpha1.ReasonReconciled, "workflow graph valid", wf.Generation)
	}

	log.V(1).Info("reconciled Workflow", "steps", len(wf.Spec.Steps))
	return ctrl.Result{}, r.Status().Update(ctx, &wf)
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Workflow{}).
		Named("workflow").
		Complete(r)
}
