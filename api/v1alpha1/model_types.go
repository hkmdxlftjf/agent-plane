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

// ModelProvider enumerates the model backends Agent Plane can describe. The control
// plane does not call these providers itself; it records how a runtime should.
// +kubebuilder:validation:Enum=openai;anthropic;azure-openai;ollama;vllm;openrouter;custom
type ModelProvider string

// ModelSpec describes a model endpoint. Agents reference a Model rather than
// embedding an endpoint/credential, so a model can be swapped or rotated in one
// place and shared across many agents.
type ModelSpec struct {
	// provider identifies the model backend family.
	// +required
	Provider ModelProvider `json:"provider"`

	// modelName is the provider-specific model identifier (e.g. "gpt-4o",
	// "claude-opus-4-8", "llama3.1:70b").
	// +kubebuilder:validation:MinLength=1
	// +required
	ModelName string `json:"modelName"`

	// endpoint is the base URL for the provider API. Optional for providers
	// with a well-known default endpoint (e.g. hosted OpenAI/Anthropic).
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// credentialRef references a Credential supplying the API key/token.
	// +optional
	CredentialRef *LocalReference `json:"credentialRef,omitempty"`

	// defaults carries provider-agnostic sampling defaults surfaced to runtimes.
	// +optional
	Defaults *ModelDefaults `json:"defaults,omitempty"`
}

// ModelDefaults holds optional default inference parameters.
type ModelDefaults struct {
	// +optional
	Temperature *string `json:"temperature,omitempty"`
	// +optional
	MaxTokens *int32 `json:"maxTokens,omitempty"`
}

// ModelStatus is the observed state of a Model.
type ModelStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.modelName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Model is the Schema for the models API.
type Model struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Model
	// +required
	Spec ModelSpec `json:"spec"`

	// status defines the observed state of Model
	// +optional
	Status ModelStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ModelList contains a list of Model
type ModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Model `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Model{}, &ModelList{})
}
