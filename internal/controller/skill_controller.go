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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/cognet/agent-plane/api/v1alpha1"
)

// SkillReconciler reconciles a Skill object — a markdown instruction pack. It
// validates that exactly one content source is set and, when the body lives in
// a ConfigMap, that the ConfigMap and key exist. It does not interpret the
// markdown; that is the runtime's job.
type SkillReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.cognet.io,resources=skills,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.cognet.io,resources=skills/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.cognet.io,resources=skills/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// Reconcile validates the Skill and resolves its content source.
func (r *SkillReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var skill corev1alpha1.Skill
	if err := r.Get(ctx, req.NamespacedName, &skill); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	skill.Status.ObservedGeneration = skill.Generation

	if err := skill.Spec.Validate(); err != nil {
		skill.Status.ContentSource = ""
		setCondition(&skill.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionFalse, corev1alpha1.ReasonInvalidSpec, err.Error(), skill.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, &skill)
	}

	// Inline body: immediately ready.
	if skill.Spec.ContentConfigMapRef == nil {
		skill.Status.ContentSource = "inline"
		setCondition(&skill.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionTrue, corev1alpha1.ReasonReconciled, "inline skill content", skill.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, &skill)
	}

	// ConfigMap-sourced body: verify the ConfigMap and key exist.
	skill.Status.ContentSource = "configMap"
	ref := skill.Spec.ContentConfigMapRef
	var cm corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Namespace: skill.Namespace, Name: ref.Name}, &cm)
	switch {
	case apierrors.IsNotFound(err):
		setCondition(&skill.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionFalse, corev1alpha1.ReasonReferenceMissing, fmt.Sprintf("configmap %q not found", ref.Name), skill.Generation)
	case err != nil:
		return ctrl.Result{}, err
	default:
		if _, ok := cm.Data[ref.Key]; ok {
			setCondition(&skill.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionTrue, corev1alpha1.ReasonReconciled, "skill content resolved from configmap", skill.Generation)
		} else {
			setCondition(&skill.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionFalse, corev1alpha1.ReasonReferenceMissing, fmt.Sprintf("key %q not present in configmap %q", ref.Key, ref.Name), skill.Generation)
		}
	}

	log.V(1).Info("reconciled Skill", "contentSource", skill.Status.ContentSource)
	return ctrl.Result{}, r.Status().Update(ctx, &skill)
}

// SetupWithManager wires the controller. It watches ConfigMaps so a Skill whose
// backing ConfigMap is created later converges to Ready.
func (r *SkillReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Skill{}).
		Watches(&corev1.ConfigMap{}, enqueueReferrers(r.Client, func() client.ObjectList { return &corev1alpha1.SkillList{} })).
		Named("skill").
		Complete(r)
}
