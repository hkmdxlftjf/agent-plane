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

// SkillSpec describes an Agent Skill in the Anthropic sense: a markdown
// instruction pack (a SKILL.md-style body) that teaches an agent *how* to do
// something, loaded via progressive disclosure. This is distinct from a Tool,
// which is an executable capability the agent *calls*. A Skill may declare the
// Tools it is allowed to use.
type SkillSpec struct {
	// description is the always-loaded one-line summary the model uses to decide
	// whether this skill is relevant (progressive disclosure, level 1).
	// +kubebuilder:validation:MinLength=1
	// +required
	Description string `json:"description"`

	// version is the semantic version of the skill content.
	// +optional
	Version string `json:"version,omitempty"`

	// content is the inline markdown body of the skill (the SKILL.md). Exactly
	// one of content or contentConfigMapRef must be set.
	// +optional
	Content string `json:"content,omitempty"`

	// contentConfigMapRef sources the markdown body from a ConfigMap key instead
	// of inlining it — useful for large skills. Exactly one of content or
	// contentConfigMapRef must be set.
	// +optional
	ContentConfigMapRef *ConfigMapKeyReference `json:"contentConfigMapRef,omitempty"`

	// allowedTools lists the names of Tools this skill is permitted to invoke.
	// The runtime enforces it; CogNet records it.
	// +optional
	// +listType=set
	AllowedTools []string `json:"allowedTools,omitempty"`
}

// SkillStatus is the observed state of a Skill.
type SkillStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// contentSource reports where the resolved body came from: "inline" or
	// "configMap".
	// +optional
	ContentSource string `json:"contentSource,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.status.contentSource`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Skill is the Schema for the skills API — a markdown instruction pack.
type Skill struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Skill
	// +required
	Spec SkillSpec `json:"spec"`

	// status defines the observed state of Skill
	// +optional
	Status SkillStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// SkillList contains a list of Skill
type SkillList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Skill `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Skill{}, &SkillList{})
}
