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

// This file holds types shared across the CogNet resource model. CogNet is a
// control plane: resources reference one another by name within a namespace,
// and the Operator resolves those references into runtime configuration served
// by the Registry. Nothing here performs inference.

// LocalReference points at another CogNet resource by name, in the same
// namespace as the referring object. This is the primary composition primitive:
// an Agent references a Model, Tools, a Workflow, etc. via LocalReference.
type LocalReference struct {
	// name is the metadata.name of the referenced resource.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// SecretKeyReference points at a key within a Kubernetes Secret. Credentials
// and connection strings are never stored inline on CogNet resources; they are
// always indirected through a Secret so that RBAC and existing secret tooling
// (sealed-secrets, external-secrets, CSI drivers) apply unchanged.
type SecretKeyReference struct {
	// name is the name of the Secret in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// key is the key within the Secret's data to select.
	// +kubebuilder:validation:MinLength=1
	// +required
	Key string `json:"key"`
}

// ConfigMapKeyReference points at a key within a Kubernetes ConfigMap. Used for
// larger, non-secret payloads such as a Skill's markdown body.
type ConfigMapKeyReference struct {
	// name is the name of the ConfigMap in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// key is the key within the ConfigMap's data to select.
	// +kubebuilder:validation:MinLength=1
	// +required
	Key string `json:"key"`
}

// Condition types used across CogNet resources.
const (
	// ConditionReady is the top-level condition summarizing whether a resource
	// has been successfully reconciled and is usable by the Registry/runtimes.
	ConditionReady = "Ready"
	// ConditionResolved indicates that all references declared by a resource
	// (e.g. an Agent's Model/Tool/Workflow refs) resolved to existing objects.
	ConditionResolved = "Resolved"
)

// Condition reasons used across CogNet resources.
const (
	ReasonReconciled       = "Reconciled"
	ReasonReferenceMissing = "ReferenceNotFound"
	ReasonProgressing      = "Progressing"
	ReasonCreateFailed     = "CreateFailed"
	ReasonInvalidSpec      = "InvalidSpec"
	ReasonSecretMissing    = "SecretNotFound"
)
