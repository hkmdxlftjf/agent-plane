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

// PromptTemplateReconciler reconciles a PromptTemplate object. A prompt has no
// external references; it is Ready once observed. Content validity (templating
// syntax) is intentionally left to runtimes, which own the templating engine.
type PromptTemplateReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=prompttemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=prompttemplates/status,verbs=get;update;patch

// Reconcile publishes readiness for the PromptTemplate.
func (r *PromptTemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pt corev1alpha1.PromptTemplate
	if err := r.Get(ctx, req.NamespacedName, &pt); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	pt.Status.ObservedGeneration = pt.Generation
	setCondition(&pt.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionTrue, corev1alpha1.ReasonReconciled, "prompt template ready", pt.Generation)
	log.V(1).Info("reconciled PromptTemplate", "version", pt.Spec.Version)
	return ctrl.Result{}, r.Status().Update(ctx, &pt)
}

// SetupWithManager sets up the controller with the Manager.
func (r *PromptTemplateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.PromptTemplate{}).
		Named("prompttemplate").
		Complete(r)
}
