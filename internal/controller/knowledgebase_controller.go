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

// KnowledgeBaseReconciler reconciles a KnowledgeBase object. It resolves the
// optional embedding Model, index Memory, and access Credential references.
type KnowledgeBaseReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=knowledgebases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=knowledgebases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=knowledgebases/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=models;memories;credentials,verbs=get;list;watch

// Reconcile validates the KnowledgeBase's references and publishes readiness.
func (r *KnowledgeBaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var kb corev1alpha1.KnowledgeBase
	if err := r.Get(ctx, req.NamespacedName, &kb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var targets []refTarget
	if kb.Spec.EmbeddingModelRef != nil {
		targets = append(targets, refTarget{kindModel, kb.Spec.EmbeddingModelRef.Name, &corev1alpha1.Model{}})
	}
	if kb.Spec.MemoryRef != nil {
		targets = append(targets, refTarget{kindMemory, kb.Spec.MemoryRef.Name, &corev1alpha1.Memory{}})
	}
	if kb.Spec.CredentialRef != nil {
		targets = append(targets, refTarget{kindCredential, kb.Spec.CredentialRef.Name, &corev1alpha1.Credential{}})
	}

	missing, err := resolveTargets(ctx, r.Client, kb.Namespace, targets)
	if err != nil {
		return ctrl.Result{}, err
	}

	kb.Status.ObservedGeneration = kb.Generation
	applyReadyFromMissing(&kb.Status.Conditions, missing, kb.Generation, "knowledge base ready")
	log.V(1).Info("reconciled KnowledgeBase", "missing", missing)
	return ctrl.Result{}, r.Status().Update(ctx, &kb)
}

// SetupWithManager sets up the controller with the Manager.
func (r *KnowledgeBaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	referrers := enqueueReferrers(r.Client, func() client.ObjectList { return &corev1alpha1.KnowledgeBaseList{} })
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.KnowledgeBase{}).
		Watches(&corev1alpha1.Model{}, referrers).
		Watches(&corev1alpha1.Memory{}, referrers).
		Watches(&corev1alpha1.Credential{}, referrers).
		Named("knowledgebase").
		Complete(r)
}
