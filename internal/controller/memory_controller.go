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

// MemoryReconciler reconciles a Memory object. A Memory is Ready once its
// optional connection Credential resolves.
type MemoryReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=memories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=memories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=memories/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=credentials,verbs=get;list;watch

// Reconcile validates the Memory's references and publishes readiness.
func (r *MemoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var memory corev1alpha1.Memory
	if err := r.Get(ctx, req.NamespacedName, &memory); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var targets []refTarget
	if memory.Spec.ConnectionRef != nil {
		targets = append(targets, refTarget{"Credential", memory.Spec.ConnectionRef.Name, &corev1alpha1.Credential{}})
	}

	missing, err := resolveTargets(ctx, r.Client, memory.Namespace, targets)
	if err != nil {
		return ctrl.Result{}, err
	}

	memory.Status.ObservedGeneration = memory.Generation
	applyReadyFromMissing(&memory.Status.Conditions, missing, memory.Generation, "memory ready")
	log.V(1).Info("reconciled Memory", "missing", missing)
	return ctrl.Result{}, r.Status().Update(ctx, &memory)
}

// SetupWithManager sets up the controller with the Manager.
func (r *MemoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Memory{}).
		Watches(&corev1alpha1.Credential{}, enqueueReferrers(r.Client, func() client.ObjectList { return &corev1alpha1.MemoryList{} })).
		Named("memory").
		Complete(r)
}
