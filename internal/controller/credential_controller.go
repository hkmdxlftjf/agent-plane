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

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

// CredentialReconciler reconciles a Credential object. It verifies that the
// referenced Secret and key actually exist, so downstream consumers (Models)
// can trust a Ready Credential instead of failing at request time.
type CredentialReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=credentials,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=credentials/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile checks the backing Secret/key and publishes readiness. It never
// reads or logs the secret value itself.
func (r *CredentialReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cred corev1alpha1.Credential
	if err := r.Get(ctx, req.NamespacedName, &cred); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	cred.Status.ObservedGeneration = cred.Generation

	var secret corev1.Secret
	secretKey := types.NamespacedName{Namespace: cred.Namespace, Name: cred.Spec.SecretRef.Name}
	err := r.Get(ctx, secretKey, &secret)
	switch {
	case apierrors.IsNotFound(err):
		r.setSecretStatus(&cred, false, fmt.Sprintf("secret %q not found", cred.Spec.SecretRef.Name))
	case err != nil:
		return ctrl.Result{}, err
	default:
		if _, ok := secret.Data[cred.Spec.SecretRef.Key]; ok {
			r.setSecretStatus(&cred, true, "secret and key present")
		} else {
			r.setSecretStatus(&cred, false, fmt.Sprintf("key %q not present in secret %q", cred.Spec.SecretRef.Key, cred.Spec.SecretRef.Name))
		}
	}

	log.V(1).Info("reconciled Credential", "secret", cred.Spec.SecretRef.Name)
	return ctrl.Result{}, r.Status().Update(ctx, &cred)
}

func (r *CredentialReconciler) setSecretStatus(cred *corev1alpha1.Credential, found bool, msg string) {
	cred.Status.SecretFound = &found
	status := metav1.ConditionFalse
	reason := corev1alpha1.ReasonSecretMissing
	if found {
		status = metav1.ConditionTrue
		reason = corev1alpha1.ReasonReconciled
	}
	setCondition(&cred.Status.Conditions, corev1alpha1.ConditionReady, status, reason, msg, cred.Generation)
}

// SetupWithManager sets up the controller with the Manager.
//
// It watches Secrets so a Credential created before its Secret converges to
// Ready once the Secret appears. Note: watching Secrets caches them in the
// manager; a production build should scope this with a label selector or
// namespace filter rather than caching every Secret in the cluster.
func (r *CredentialReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Credential{}).
		Watches(&corev1.Secret{},
			enqueueByIndex(r.Client, func() client.ObjectList { return &corev1alpha1.CredentialList{} }, idxCredentialSecret)).
		Named("credential").
		Complete(r)
}
