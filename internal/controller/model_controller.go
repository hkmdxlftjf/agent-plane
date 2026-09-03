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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

// ModelReconciler reconciles a Model object. A Model is Ready once its optional
// Credential reference (if any) resolves; runtimes read the Model to learn the
// provider/endpoint without the Agent ever embedding an endpoint.
type ModelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=models,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=models/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=credentials,verbs=get;list;watch

// Reconcile validates the Model's references and publishes readiness.
func (r *ModelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var model corev1alpha1.Model
	if err := r.Get(ctx, req.NamespacedName, &model); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var targets []refTarget
	if model.Spec.CredentialRef != nil {
		targets = append(targets, refTarget{kindCredential, model.Spec.CredentialRef.Name, &corev1alpha1.Credential{}})
	}

	missing, err := resolveTargets(ctx, r.Client, model.Namespace, targets)
	if err != nil {
		return ctrl.Result{}, err
	}

	model.Status.ObservedGeneration = model.Generation
	applyReadyFromMissing(&model.Status.Conditions, missing, model.Generation, "model ready")
	log.V(1).Info("reconciled Model", "missing", missing)
	return ctrl.Result{}, r.Status().Update(ctx, &model)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Model{}).
		Watches(&corev1alpha1.Credential{},
			enqueueByIndex(r.Client, func() client.ObjectList { return &corev1alpha1.ModelList{} }, idxModelCredential)).
		Named("model").
		Complete(r)
}

// applyReadyFromMissing is the shared status-writing tail for the
// reference-resolving controllers: if any references are missing it sets
// Resolved/Ready False with reason ReferenceNotFound, otherwise both True.
func applyReadyFromMissing(conds *[]metav1.Condition, missing []string, generation int64, readyMsg string) {
	if len(missing) > 0 {
		msg := fmt.Sprintf("unresolved references: %v", missing)
		setCondition(conds, corev1alpha1.ConditionResolved, metav1.ConditionFalse, corev1alpha1.ReasonReferenceMissing, msg, generation)
		setCondition(conds, corev1alpha1.ConditionReady, metav1.ConditionFalse, corev1alpha1.ReasonReferenceMissing, msg, generation)
		return
	}
	setCondition(conds, corev1alpha1.ConditionResolved, metav1.ConditionTrue, corev1alpha1.ReasonReconciled, "all references resolved", generation)
	setCondition(conds, corev1alpha1.ConditionReady, metav1.ConditionTrue, corev1alpha1.ReasonReconciled, readyMsg, generation)
}
