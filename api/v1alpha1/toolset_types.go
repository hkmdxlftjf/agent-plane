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

// ToolSetSpec is a named, reusable bundle of Tools. Agents reference a
// ToolSet to gain all its tools at once, so common capability groupings can
// be shared and versioned in one place.
type ToolSetSpec struct {
	// description documents the purpose of this tool bundle.
	// +optional
	Description string `json:"description,omitempty"`

	// toolRefs lists the Tools included in this set.
	// +required
	// +listType=atomic
	ToolRefs []LocalReference `json:"toolRefs"`
}

// ToolSetStatus is the observed state of a ToolSet.
type ToolSetStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// resolvedTools is the count of member Tools that currently exist.
	// +optional
	ResolvedTools int `json:"resolvedTools,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ss
// +kubebuilder:printcolumn:name="Tools",type=integer,JSONPath=`.status.resolvedTools`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ToolSet is the Schema for the toolsets API.
type ToolSet struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of ToolSet
	// +required
	Spec ToolSetSpec `json:"spec"`

	// status defines the observed state of ToolSet
	// +optional
	Status ToolSetStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ToolSetList contains a list of ToolSet
type ToolSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ToolSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ToolSet{}, &ToolSetList{})
}
