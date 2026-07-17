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

// MemoryBackend enumerates the storage backends a Memory can describe.
// +kubebuilder:validation:Enum=redis;postgres;vector;graph;s3
type MemoryBackend string

// MemorySpec describes a memory/storage backend an Agent may use. The control
// plane records the backend and where its connection details live; the runtime
// decides how to read/write it.
type MemorySpec struct {
	// backend identifies the storage technology.
	// +required
	Backend MemoryBackend `json:"backend"`

	// connectionRef references a Credential holding connection details
	// (DSN, URL, access keys) for the backend.
	// +optional
	ConnectionRef *LocalReference `json:"connectionRef,omitempty"`

	// namespace optionally scopes/prefixes entries within the backend (e.g. a
	// Redis key prefix or a vector collection name).
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// MemoryStatus is the observed state of a Memory.
type MemoryStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.backend`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Memory is the Schema for the memories API.
type Memory struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Memory
	// +required
	Spec MemorySpec `json:"spec"`

	// status defines the observed state of Memory
	// +optional
	Status MemoryStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// MemoryList contains a list of Memory
type MemoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Memory `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Memory{}, &MemoryList{})
}
