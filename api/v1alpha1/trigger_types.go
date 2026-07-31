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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TriggerSpec declares an *inbound* event source for an Agent: something that
// brings messages in, as opposed to a Tool, which the Agent calls out to.
//
// Agent Plane does not implement any platform integration itself. It runs an
// adapter image you supply and injects a fixed contract (see
// docs/adapter-protocol.md); the adapter connects to Lark/DingTalk/Slack/…,
// turns each message into a call on the Agent's runtime, and posts the answer
// back. Adding a platform is a new adapter image and a new Trigger — no
// control-plane change.
type TriggerSpec struct {
	// agentRef is the Agent this trigger feeds. The Operator resolves the
	// Agent's runtime Service and injects its address as
	// AGENTPLANE_AGENT_ENDPOINT, so the adapter never needs to know how the
	// runtime is addressed.
	// +required
	AgentRef LocalReference `json:"agentRef"`

	// image is the adapter container image. User-supplied: Agent Plane schedules
	// it and wires the contract, but implements no platform protocol itself.
	// +kubebuilder:validation:MinLength=1
	// +required
	Image string `json:"image"`

	// credentialRef references a Credential whose Secret holds the platform
	// credentials (app id/secret, bot token, …). The Secret is mounted into the
	// adapter at AGENTPLANE_CREDENTIAL_PATH rather than passed as environment,
	// so it does not show up in `kubectl describe pod` or process listings.
	// +optional
	CredentialRef *LocalReference `json:"credentialRef,omitempty"`

	// config is opaque adapter configuration, serialized to
	// AGENTPLANE_TRIGGER_CONFIG verbatim. Its shape is the adapter's business —
	// which events to subscribe to, which chats to answer in, and so on — so the
	// control plane can stay platform-neutral.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Config *runtime.RawExtension `json:"config,omitempty"`

	// replicas is the desired number of adapter pods.
	//
	// A streaming adapter holds one long-lived connection to the platform, and
	// most platforms deliver each event to every connection — so running more
	// than one replica usually means answering each message more than once.
	// Leave this at 1 unless the adapter documents that it shards or elects a
	// leader.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// env is extra environment for the adapter container. The Operator always
	// injects the contract variables (AGENTPLANE_AGENT_ENDPOINT,
	// AGENTPLANE_AGENT_NAME, AGENTPLANE_AGENT_NAMESPACE,
	// AGENTPLANE_TRIGGER_NAME, AGENTPLANE_TRIGGER_CONFIG, and
	// AGENTPLANE_CREDENTIAL_PATH when a credential is referenced); an entry here
	// with one of those names is ignored rather than allowed to break the
	// contract.
	// +optional
	// +listType=atomic
	Env []corev1.EnvVar `json:"env,omitempty"`

	// resources is the adapter container's resource requirements.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// TriggerPhase is a coarse, human-facing summary of a Trigger's state.
//
// Note what Running does *not* claim: the Operator can see that the adapter pod
// is up, but whether the adapter has actually authenticated and connected to the
// platform is only knowable inside the adapter. Reporting that would need the
// adapter to report it back, which the contract deliberately does not require —
// so Running means "the adapter is scheduled and its probe passes", nothing more.
// +kubebuilder:validation:Enum=Pending;Running;Degraded
type TriggerPhase string

const (
	TriggerPhasePending  TriggerPhase = "Pending"
	TriggerPhaseRunning  TriggerPhase = "Running"
	TriggerPhaseDegraded TriggerPhase = "Degraded"
)

// TriggerStatus is the observed state of a Trigger.
type TriggerStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Phase TriggerPhase `json:"phase,omitempty"`
	// availableReplicas mirrors the adapter Deployment.
	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`
	// agentEndpoint is the address injected into the adapter, echoed here so an
	// operator can see what the adapter was told to call.
	// +optional
	AgentEndpoint string `json:"agentEndpoint,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=trg
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Trigger is the Schema for the triggers API — an inbound event source feeding
// an Agent.
type Trigger struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Trigger
	// +required
	Spec TriggerSpec `json:"spec"`

	// status defines the observed state of Trigger
	// +optional
	Status TriggerStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// TriggerList contains a list of Trigger
type TriggerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Trigger `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Trigger{}, &TriggerList{})
}
