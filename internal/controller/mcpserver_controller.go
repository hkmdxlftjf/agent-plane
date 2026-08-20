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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

// MCPServerReconciler reconciles an MCPServer object. It materializes the
// declared MCP server into a Deployment + Service it owns, so that Agents can
// reference the MCPServer without ever dealing with workloads directly.
type MCPServerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=mcpservers,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=mcpservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch

// Reconcile ensures a Deployment and Service exist for the MCPServer and
// reflects the Deployment's availability into status.
func (r *MCPServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var mcp corev1alpha1.MCPServer
	if err := r.Get(ctx, req.NamespacedName, &mcp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// External mode: the MCP server lives outside the cluster. No workload is
	// materialized; the declared URL is the endpoint.
	if mcp.Spec.ExternalEndpoint != "" {
		if err := r.deleteManaged(ctx, &mcp); err != nil {
			return ctrl.Result{}, err
		}
		mcp.Status.ObservedGeneration = mcp.Generation
		mcp.Status.Endpoint = mcp.Spec.ExternalEndpoint
		mcp.Status.AvailableReplicas = 0
		setCondition(&mcp.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionTrue, corev1alpha1.ReasonReconciled, "external endpoint", mcp.Generation)
		log.Info("reconciled MCPServer", "external", mcp.Spec.ExternalEndpoint)
		return ctrl.Result{}, r.Status().Update(ctx, &mcp)
	}

	if err := r.reconcileDeployment(ctx, &mcp); err != nil {
		setCondition(&mcp.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionFalse, corev1alpha1.ReasonCreateFailed, err.Error(), mcp.Generation)
		_ = r.Status().Update(ctx, &mcp)
		return ctrl.Result{}, err
	}
	if err := r.reconcileService(ctx, &mcp); err != nil {
		setCondition(&mcp.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionFalse, corev1alpha1.ReasonCreateFailed, err.Error(), mcp.Generation)
		_ = r.Status().Update(ctx, &mcp)
		return ctrl.Result{}, err
	}

	// Refresh availability from the managed Deployment.
	var dep appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{Namespace: mcp.Namespace, Name: mcp.Name}, &dep); err == nil {
		mcp.Status.AvailableReplicas = dep.Status.AvailableReplicas
	}

	mcp.Status.ObservedGeneration = mcp.Generation
	mcp.Status.Endpoint = fmt.Sprintf("http://%s.%s.svc:%d", mcp.Name, mcp.Namespace, servicePort(&mcp))

	ready := mcp.Spec.Replicas > 0 && mcp.Status.AvailableReplicas >= mcp.Spec.Replicas
	if ready {
		setCondition(&mcp.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionTrue, corev1alpha1.ReasonReconciled, "deployment available", mcp.Generation)
	} else {
		setCondition(&mcp.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionFalse, corev1alpha1.ReasonProgressing, "waiting for deployment to become available", mcp.Generation)
	}

	log.Info("reconciled MCPServer", "available", mcp.Status.AvailableReplicas, "desired", mcp.Spec.Replicas)
	return ctrl.Result{}, r.Status().Update(ctx, &mcp)
}

func mcpLabels(mcp *corev1alpha1.MCPServer) map[string]string {
	return ownedLabels("mcpserver", mcp.Name)
}

func servicePort(mcp *corev1alpha1.MCPServer) int32 {
	if mcp.Spec.Port != 0 {
		return mcp.Spec.Port
	}
	return 8080
}

// deleteManaged removes the owned Deployment/Service, for an MCPServer that
// switched from image mode to externalEndpoint mode.
func (r *MCPServerReconciler) deleteManaged(ctx context.Context, mcp *corev1alpha1.MCPServer) error {
	key := client.ObjectKey{Namespace: mcp.Namespace, Name: mcp.Name}
	var dep appsv1.Deployment
	if err := r.Get(ctx, key, &dep); err == nil && metav1.IsControlledBy(&dep, mcp) {
		if err := r.Delete(ctx, &dep); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	var svc corev1.Service
	if err := r.Get(ctx, key, &svc); err == nil && metav1.IsControlledBy(&svc, mcp) {
		if err := r.Delete(ctx, &svc); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// reconcileDeployment creates or updates the Deployment owned by the MCPServer.
func (r *MCPServerReconciler) reconcileDeployment(ctx context.Context, mcp *corev1alpha1.MCPServer) error {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: mcp.Name, Namespace: mcp.Namespace}}
	labels := mcpLabels(mcp)
	port := servicePort(mcp)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		replicas := mcp.Spec.Replicas
		dep.Labels = labels
		dep.Spec.Replicas = &replicas
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		dep.Spec.Template.Labels = labels
		dep.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:      portNameMCP,
			Image:     mcp.Spec.Image,
			Env:       mcp.Spec.Env,
			Resources: mcp.Spec.Resources,
			Ports: []corev1.ContainerPort{{
				Name:          portNameMCP,
				ContainerPort: port,
			}},
		}}
		return controllerutil.SetControllerReference(mcp, dep, r.Scheme)
	})
	return err
}

// reconcileService creates or updates the Service owned by the MCPServer.
func (r *MCPServerReconciler) reconcileService(ctx context.Context, mcp *corev1alpha1.MCPServer) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: mcp.Name, Namespace: mcp.Namespace}}
	labels := mcpLabels(mcp)
	port := servicePort(mcp)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Selector = labels
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       portNameMCP,
			Port:       port,
			TargetPort: intstr.FromInt32(port),
			Protocol:   corev1.ProtocolTCP,
		}}
		return controllerutil.SetControllerReference(mcp, svc, r.Scheme)
	})
	return err
}

// SetupWithManager wires the controller to own the Deployment and Service so
// that changes to them re-trigger reconciliation.
func (r *MCPServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.MCPServer{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Named("mcpserver").
		Complete(r)
}
