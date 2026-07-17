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

// ToolReconciler reconciles a Tool object. It validates the tool's contract
// (e.g. an mcp tool must reference an MCPServer) and resolves that reference.
type ToolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.cognet.io,resources=tools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.cognet.io,resources=tools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.cognet.io,resources=tools/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.cognet.io,resources=mcpservers,verbs=get;list;watch

// Reconcile validates the Tool and publishes readiness.
func (r *ToolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var tool corev1alpha1.Tool
	if err := r.Get(ctx, req.NamespacedName, &tool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	tool.Status.ObservedGeneration = tool.Generation

	// Structural validation (also enforced at admission by the webhook).
	if err := tool.Spec.Validate(); err != nil {
		setCondition(&tool.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionFalse, corev1alpha1.ReasonInvalidSpec, err.Error(), tool.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, &tool)
	}

	var targets []refTarget
	if tool.Spec.MCPServerRef != nil {
		targets = append(targets, refTarget{"MCPServer", tool.Spec.MCPServerRef.Name, &corev1alpha1.MCPServer{}})
	}

	missing, err := resolveTargets(ctx, r.Client, tool.Namespace, targets)
	if err != nil {
		return ctrl.Result{}, err
	}

	applyReadyFromMissing(&tool.Status.Conditions, missing, tool.Generation, "tool ready")
	log.V(1).Info("reconciled Tool", "type", tool.Spec.Type, "missing", missing)
	return ctrl.Result{}, r.Status().Update(ctx, &tool)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ToolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Tool{}).
		Watches(&corev1alpha1.MCPServer{}, enqueueReferrers(r.Client, func() client.ObjectList { return &corev1alpha1.ToolList{} })).
		Named("tool").
		Complete(r)
}
