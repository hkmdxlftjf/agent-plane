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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentClassSpec provides shared defaults that Agents can inherit, analogous to
// how a StorageClass provides defaults for PersistentVolumeClaims. It reduces
// repetition across many similarly-shaped agents.
type AgentClassSpec struct {
	// description documents the class of agents this defines.
	// +optional
	Description string `json:"description,omitempty"`

	// defaultModelRef is applied to Agents of this class that omit a modelRef.
	// +optional
	DefaultModelRef *LocalReference `json:"defaultModelRef,omitempty"`

	// defaultWorkflowRef is applied when an Agent omits a workflowRef.
	// +optional
	DefaultWorkflowRef *LocalReference `json:"defaultWorkflowRef,omitempty"`

	// defaultPromptRef is applied when an Agent omits a promptRef.
	// +optional
	DefaultPromptRef *LocalReference `json:"defaultPromptRef,omitempty"`

	// defaultPolicyRefs are merged into every Agent of this class.
	// +optional
	// +listType=atomic
	DefaultPolicyRefs []LocalReference `json:"defaultPolicyRefs,omitempty"`
}

// AgentClassStatus is the observed state of an AgentClass.
type AgentClassStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=agc
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentClass is the Schema for the agentclasses API.
type AgentClass struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of AgentClass
	// +required
	Spec AgentClassSpec `json:"spec"`

	// status defines the observed state of AgentClass
	// +optional
	Status AgentClassStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// AgentClassList contains a list of AgentClass
type AgentClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentClass{}, &AgentClassList{})
}
