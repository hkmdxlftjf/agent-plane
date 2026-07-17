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

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

// ToolSetReconciler reconciles a ToolSet object. It resolves every member
// Tool and reports how many currently exist; the set is Ready only when all
// members resolve.
type ToolSetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.cognet.io,resources=toolsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.cognet.io,resources=toolsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.cognet.io,resources=toolsets/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.cognet.io,resources=tools,verbs=get;list;watch

// Reconcile resolves the ToolSet's member Tools and publishes readiness.
func (r *ToolSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var set corev1alpha1.ToolSet
	if err := r.Get(ctx, req.NamespacedName, &set); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	targets := make([]refTarget, 0, len(set.Spec.ToolRefs))
	for _, ref := range set.Spec.ToolRefs {
		targets = append(targets, refTarget{"Tool", ref.Name, &corev1alpha1.Tool{}})
	}

	missing, err := resolveTargets(ctx, r.Client, set.Namespace, targets)
	if err != nil {
		return ctrl.Result{}, err
	}

	set.Status.ObservedGeneration = set.Generation
	set.Status.ResolvedTools = len(set.Spec.ToolRefs) - len(missing)
	applyReadyFromMissing(&set.Status.Conditions, missing, set.Generation, "all member tools resolved")
	log.V(1).Info("reconciled ToolSet", "resolved", set.Status.ResolvedTools, "missing", missing)
	return ctrl.Result{}, r.Status().Update(ctx, &set)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ToolSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.ToolSet{}).
		Watches(&corev1alpha1.Tool{}, enqueueReferrers(r.Client, func() client.ObjectList { return &corev1alpha1.ToolSetList{} })).
		Named("toolset").
		Complete(r)
}
