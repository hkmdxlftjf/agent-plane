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

// CredentialSpec indirects secret material through a Kubernetes Secret. Agent Plane
// never stores secrets inline; Models and MCP servers reference a
// Credential, which in turn points at a Secret key.
type CredentialSpec struct {
	// secretRef points at the Secret key holding the credential value.
	// +required
	SecretRef SecretKeyReference `json:"secretRef"`

	// description documents what this credential is for.
	// +optional
	Description string `json:"description,omitempty"`
}

// CredentialStatus is the observed state of a Credential.
type CredentialStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// secretFound reflects whether the referenced Secret/key was located.
	// +optional
	SecretFound *bool `json:"secretFound,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cred
// +kubebuilder:printcolumn:name="Secret",type=string,JSONPath=`.spec.secretRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Credential is the Schema for the credentials API.
type Credential struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Credential
	// +required
	Spec CredentialSpec `json:"spec"`

	// status defines the observed state of Credential
	// +optional
	Status CredentialStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// CredentialList contains a list of Credential
type CredentialList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Credential `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Credential{}, &CredentialList{})
}
