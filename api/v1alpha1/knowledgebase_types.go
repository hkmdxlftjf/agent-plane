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

// KnowledgeBaseSource enumerates where a KnowledgeBase draws content from.
// +kubebuilder:validation:Enum=s3;http;git;vector
type KnowledgeBaseSource string

// KnowledgeBaseSpec describes a corpus of documents an Agent can retrieve from
// (RAG). Like Memory, the control plane records where the corpus lives and how
// it is indexed; the runtime performs retrieval.
type KnowledgeBaseSpec struct {
	// source is the type of backing store for the corpus.
	// +required
	Source KnowledgeBaseSource `json:"source"`

	// uri locates the corpus (bucket, endpoint, or repository URL).
	// +optional
	URI string `json:"uri,omitempty"`

	// embeddingModelRef references the Model used to embed documents/queries.
	// +optional
	EmbeddingModelRef *LocalReference `json:"embeddingModelRef,omitempty"`

	// memoryRef references the Memory (e.g. a vector backend) storing the index.
	// +optional
	MemoryRef *LocalReference `json:"memoryRef,omitempty"`

	// credentialRef references credentials for accessing the source.
	// +optional
	CredentialRef *LocalReference `json:"credentialRef,omitempty"`
}

// KnowledgeBaseStatus is the observed state of a KnowledgeBase.
type KnowledgeBaseStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=kb
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KnowledgeBase is the Schema for the knowledgebases API.
type KnowledgeBase struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of KnowledgeBase
	// +required
	Spec KnowledgeBaseSpec `json:"spec"`

	// status defines the observed state of KnowledgeBase
	// +optional
	Status KnowledgeBaseStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// KnowledgeBaseList contains a list of KnowledgeBase
type KnowledgeBaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KnowledgeBase `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KnowledgeBase{}, &KnowledgeBaseList{})
}
