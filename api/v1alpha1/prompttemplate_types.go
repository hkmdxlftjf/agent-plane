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

// PromptTemplateSpec manages reusable prompt content (system prompts, role
// prompts, few-shot examples, templates) so multiple Agents can share and
// version prompts independently of code.
type PromptTemplateSpec struct {
	// version is the semantic version of this prompt.
	// +optional
	Version string `json:"version,omitempty"`

	// system is the system prompt text.
	// +optional
	System string `json:"system,omitempty"`

	// roles carries additional role-scoped prompts keyed by role name.
	// +optional
	Roles []RolePrompt `json:"roles,omitempty"`

	// fewShot is an ordered list of few-shot examples.
	// +optional
	// +listType=atomic
	FewShot []FewShotExample `json:"fewShot,omitempty"`

	// template is the primary prompt template body (engine-specific templating).
	// +optional
	Template string `json:"template,omitempty"`
}

// RolePrompt is a prompt scoped to a specific role.
type RolePrompt struct {
	// +kubebuilder:validation:MinLength=1
	// +required
	Role string `json:"role"`
	// +required
	Content string `json:"content"`
}

// FewShotExample is a single input/output demonstration.
type FewShotExample struct {
	// +required
	Input string `json:"input"`
	// +required
	Output string `json:"output"`
}

// PromptTemplateStatus is the observed state of a PromptTemplate.
type PromptTemplateStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pt
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PromptTemplate is the Schema for the prompttemplates API.
type PromptTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of PromptTemplate
	// +required
	Spec PromptTemplateSpec `json:"spec"`

	// status defines the observed state of PromptTemplate
	// +optional
	Status PromptTemplateStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// PromptTemplateList contains a list of PromptTemplate
type PromptTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PromptTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PromptTemplate{}, &PromptTemplateList{})
}
