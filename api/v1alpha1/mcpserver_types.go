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
)

// MCPTransport enumerates how an MCP server exposes its protocol.
// +kubebuilder:validation:Enum=stdio;http;sse
type MCPTransport string

// MCPServerSpec describes an MCP (Model Context Protocol) server. The Operator
// materializes this into a Deployment + Service; Agents only reference the
// MCPServer and never deal with workloads directly. Alternatively,
// externalEndpoint declares an MCP server hosted outside the cluster (e.g. a
// vendor's managed endpoint) — no workload is created and the endpoint is
// published to status as-is.
// +kubebuilder:validation:XValidation:rule="(has(self.image) && !has(self.externalEndpoint)) || (!has(self.image) && has(self.externalEndpoint))",message="exactly one of image or externalEndpoint must be set"
type MCPServerSpec struct {
	// image is the container image implementing the MCP server. Mutually
	// exclusive with externalEndpoint.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Image string `json:"image,omitempty"`

	// externalEndpoint is the URL of an MCP server hosted outside the cluster.
	// When set, no Deployment/Service is created and this URL is published to
	// status.endpoint directly. Note: a URL with query parameters (e.g. an
	// api key) is visible to anyone who can read this resource — prefer
	// self-hosting (image) when the key must stay in a Secret.
	// +kubebuilder:validation:Pattern=`^https?://.+`
	// +optional
	ExternalEndpoint string `json:"externalEndpoint,omitempty"`

	// transport is the protocol transport the server exposes.
	// +kubebuilder:default=http
	// +optional
	Transport MCPTransport `json:"transport,omitempty"`

	// port is the container/service port the server listens on.
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// replicas is the desired number of server pods.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// env is passed to the server container.
	// +optional
	// +listType=atomic
	Env []corev1.EnvVar `json:"env,omitempty"`

	// resources is the container resource requirements.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// MCPServerStatus is the observed state of an MCPServer.
type MCPServerStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// endpoint is the in-cluster address runtimes use to reach this server.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// availableReplicas mirrors the managed Deployment's available replicas.
	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mcp
// +kubebuilder:printcolumn:name="Transport",type=string,JSONPath=`.spec.transport`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MCPServer is the Schema for the mcpservers API.
type MCPServer struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of MCPServer
	// +required
	Spec MCPServerSpec `json:"spec"`

	// status defines the observed state of MCPServer
	// +optional
	Status MCPServerStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// MCPServerList contains a list of MCPServer
type MCPServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MCPServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MCPServer{}, &MCPServerList{})
}
