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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/hkmdxlftjf/agent-plane/api/v1alpha1"
)

// TriggerReconciler reconciles a Trigger — an inbound event source for an
// Agent. It follows the resource-owning pattern of MCPServerReconciler: it
// materializes an adapter Deployment it owns, and wires the adapter contract
// (see docs/adapter-protocol.md) into the container so the adapter needs to know
// nothing about Agent Plane's internals.
type TriggerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// credentialMountPath is where a referenced Credential's Secret is mounted in
// the adapter container. Mounting rather than passing environment keeps the
// value out of `kubectl describe pod` and the process environment.
const credentialMountPath = "/var/run/agentplane/credential"

// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=triggers,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=triggers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.hkmdxlftjf.io,resources=agents;credentials,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

// Reconcile resolves the Trigger's Agent, materializes the adapter Deployment,
// and publishes readiness.
func (r *TriggerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var trigger corev1alpha1.Trigger
	if err := r.Get(ctx, req.NamespacedName, &trigger); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	trigger.Status.ObservedGeneration = trigger.Generation

	if err := trigger.Spec.Validate(); err != nil {
		return ctrl.Result{}, r.degrade(ctx, &trigger, corev1alpha1.ReasonInvalidSpec, err.Error())
	}

	// Resolve where the adapter should send messages. This is the one piece of
	// Agent Plane knowledge the adapter must not need: it is handed an address.
	endpoint, err := r.agentEndpoint(ctx, &trigger)
	if err != nil {
		return ctrl.Result{}, r.degrade(ctx, &trigger, corev1alpha1.ReasonReferenceMissing, err.Error())
	}
	trigger.Status.AgentEndpoint = endpoint

	// A referenced Credential must resolve to a Secret name before the adapter
	// can be scheduled with the mount.
	secretName := ""
	if ref := trigger.Spec.CredentialRef; ref != nil {
		var cred corev1alpha1.Credential
		switch err := r.Get(ctx, types.NamespacedName{Namespace: trigger.Namespace, Name: ref.Name}, &cred); {
		case apierrors.IsNotFound(err):
			return ctrl.Result{}, r.degrade(ctx, &trigger, corev1alpha1.ReasonReferenceMissing,
				fmt.Sprintf("credential %q not found", ref.Name))
		case err != nil:
			return ctrl.Result{}, err
		}
		secretName = cred.Spec.SecretRef.Name
	}

	if err := r.reconcileAdapter(ctx, &trigger, endpoint, secretName); err != nil {
		return ctrl.Result{}, err
	}

	// Reflect availability. Running says the adapter pod is up and its probe
	// passes — not that it has authenticated with the platform, which only the
	// adapter can know.
	var live appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{Namespace: trigger.Namespace, Name: adapterName(&trigger)}, &live); err == nil {
		trigger.Status.AvailableReplicas = live.Status.AvailableReplicas
	}
	if trigger.Status.AvailableReplicas > 0 {
		trigger.Status.Phase = corev1alpha1.TriggerPhaseRunning
		setCondition(&trigger.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionTrue,
			corev1alpha1.ReasonReconciled, "adapter running", trigger.Generation)
	} else {
		trigger.Status.Phase = corev1alpha1.TriggerPhasePending
		setCondition(&trigger.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionFalse,
			corev1alpha1.ReasonProgressing, "waiting for the adapter to become available", trigger.Generation)
	}

	log.V(1).Info("reconciled Trigger", "endpoint", endpoint, "available", trigger.Status.AvailableReplicas)
	return ctrl.Result{}, r.Status().Update(ctx, &trigger)
}

// degrade marks the Trigger Degraded and persists the status.
func (r *TriggerReconciler) degrade(ctx context.Context, trigger *corev1alpha1.Trigger, reason, msg string) error {
	trigger.Status.Phase = corev1alpha1.TriggerPhaseDegraded
	setCondition(&trigger.Status.Conditions, corev1alpha1.ConditionReady, metav1.ConditionFalse, reason, msg, trigger.Generation)
	return r.Status().Update(ctx, trigger)
}

// agentEndpoint resolves the in-cluster address of the Agent's runtime.
//
// The adapter delivers messages over HTTP, so the Agent must expose one: that
// means spec.runtime with a port, which is what makes the Operator create the
// runtime Service. An Agent without it is a configuration error worth reporting
// here — injecting an address nothing listens on would surface much later as an
// adapter that connects to the platform and then silently fails every message.
func (r *TriggerReconciler) agentEndpoint(ctx context.Context, trigger *corev1alpha1.Trigger) (string, error) {
	name := trigger.Spec.AgentRef.Name
	var agent corev1alpha1.Agent
	switch err := r.Get(ctx, types.NamespacedName{Namespace: trigger.Namespace, Name: name}, &agent); {
	case apierrors.IsNotFound(err):
		return "", fmt.Errorf("agent %q not found", name)
	case err != nil:
		return "", err
	}
	if agent.Spec.Runtime == nil {
		return "", fmt.Errorf("agent %q has no spec.runtime, so it exposes no endpoint for an adapter to call", name)
	}
	if agent.Spec.Runtime.Port == 0 {
		return "", fmt.Errorf("agent %q has spec.runtime but no port, so no Service is created for an adapter to call", name)
	}
	// Matches the Service the Agent controller materializes.
	return fmt.Sprintf("http://%s-runtime.%s.svc:%d", agent.Name, agent.Namespace, agent.Spec.Runtime.Port), nil
}

func adapterName(trigger *corev1alpha1.Trigger) string {
	return trigger.Name + "-adapter"
}

func adapterLabels(trigger *corev1alpha1.Trigger) map[string]string {
	return ownedLabels("agent-plane-adapter", trigger.Name)
}

// reconcileAdapter materializes the adapter Deployment the Trigger owns. No
// Service is created: a streaming adapter dials out to the platform and is not
// dialed into.
func (r *TriggerReconciler) reconcileAdapter(ctx context.Context, trigger *corev1alpha1.Trigger, endpoint, secretName string) error {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: adapterName(trigger), Namespace: trigger.Namespace}}
	labels := adapterLabels(trigger)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		replicas := trigger.Spec.Replicas
		dep.Labels = labels
		dep.Spec.Replicas = &replicas
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		dep.Spec.Template.Labels = labels

		// An adapter holds a long connection, and platforms deliver each event to
		// *every* open connection. RollingUpdate starts the replacement before the
		// old pod exits, so during any update two connections are live and the user
		// gets every message answered twice — the exact failure spec.replicas is
		// capped at 1 to prevent, reintroduced on each rollout.
		dep.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}

		container := corev1.Container{
			Name:      "adapter",
			Image:     trigger.Spec.Image,
			Env:       adapterEnv(trigger, endpoint, secretName),
			Resources: trigger.Spec.Resources,
		}
		if secretName != "" {
			container.VolumeMounts = []corev1.VolumeMount{{
				Name:      "credential",
				MountPath: credentialMountPath,
				ReadOnly:  true,
			}}
			dep.Spec.Template.Spec.Volumes = []corev1.Volume{{
				Name: "credential",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: secretName},
				},
			}}
		} else {
			// Clear a mount left behind by a credentialRef that was removed.
			dep.Spec.Template.Spec.Volumes = nil
		}
		dep.Spec.Template.Spec.Containers = []corev1.Container{container}
		return controllerutil.SetControllerReference(trigger, dep, r.Scheme)
	})
	return err
}

// adapterEnv builds the contract environment. The injected variables come last
// so a user-supplied entry cannot shadow them — spec.Validate already rejects
// that, but the ordering keeps the guarantee even if validation is bypassed.
func adapterEnv(trigger *corev1alpha1.Trigger, endpoint, secretName string) []corev1.EnvVar {
	env := append([]corev1.EnvVar{}, trigger.Spec.Env...)
	env = append(env,
		corev1.EnvVar{Name: "AGENTPLANE_AGENT_ENDPOINT", Value: endpoint},
		corev1.EnvVar{Name: "AGENTPLANE_AGENT_NAME", Value: trigger.Spec.AgentRef.Name},
		corev1.EnvVar{Name: "AGENTPLANE_AGENT_NAMESPACE", Value: trigger.Namespace},
		corev1.EnvVar{Name: "AGENTPLANE_TRIGGER_NAME", Value: trigger.Name},
	)
	if cfg := trigger.Spec.Config; cfg != nil && len(cfg.Raw) > 0 {
		env = append(env, corev1.EnvVar{Name: "AGENTPLANE_TRIGGER_CONFIG", Value: string(cfg.Raw)})
	}
	if secretName != "" {
		env = append(env, corev1.EnvVar{Name: "AGENTPLANE_CREDENTIAL_PATH", Value: credentialMountPath})
	}
	return env
}

// SetupWithManager wires the controller. It watches the Agent it feeds, so a
// runtime port appearing or changing re-reconciles the adapter's endpoint, and
// the Credential, so a Secret rename reaches the mount.
//
// SetupFieldIndexes must have run on this manager first.
func (r *TriggerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	triggers := func() client.ObjectList { return &corev1alpha1.TriggerList{} }
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Trigger{}).
		Owns(&appsv1.Deployment{}).
		Watches(&corev1alpha1.Agent{}, enqueueByIndex(r.Client, triggers, idxTriggerAgent)).
		Watches(&corev1alpha1.Credential{}, enqueueByIndex(r.Client, triggers, idxTriggerCredential)).
		Named("trigger").
		Complete(r)
}
