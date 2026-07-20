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
	"k8s.io/apimachinery/pkg/runtime"
)

// ToolType enumerates how a Tool is invoked. Agent Plane only records the
// contract; the runtime performs the actual call.
// +kubebuilder:validation:Enum=http;grpc;mcp;wasm;plugin;container
type ToolType string

// ToolSpec describes a single executable capability an Agent can invoke.
type ToolSpec struct {
	// type is the invocation mechanism for this tool.
	// +required
	Type ToolType `json:"type"`

	// description explains what the tool does, so a model can decide when to
	// call it. Surfaced to runtimes as the tool-call description.
	// +optional
	Description string `json:"description,omitempty"`

	// version is the semantic version of the tool contract.
	// +optional
	Version string `json:"version,omitempty"`

	// mcpToolName, for type=mcp, is the tool name to invoke on the MCP server
	// when it differs from this resource's metadata.name.
	// +optional
	MCPToolName string `json:"mcpToolName,omitempty"`

	// endpoint is the address the runtime uses to reach the tool. Its meaning
	// depends on type (URL for http/grpc, image ref for container, etc.).
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// mcpServerRef, for type=mcp, references the MCPServer exposing this tool.
	// +optional
	MCPServerRef *LocalReference `json:"mcpServerRef,omitempty"`

	// timeoutSeconds bounds a single tool invocation.
	// +kubebuilder:validation:Minimum=1
	// +optional
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`

	// inputSchema is a JSON Schema for the tool's input arguments.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	InputSchema *runtime.RawExtension `json:"inputSchema,omitempty"`

	// outputSchema is a JSON Schema for the tool's result.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	OutputSchema *runtime.RawExtension `json:"outputSchema,omitempty"`
}

// ToolStatus is the observed state of a Tool.
type ToolStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// healthy reflects the last observed health probe result, if any.
	// +optional
	Healthy *bool `json:"healthy,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Tool is the Schema for the tools API.
type Tool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Tool
	// +required
	Spec ToolSpec `json:"spec"`

	// status defines the observed state of Tool
	// +optional
	Status ToolStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ToolList contains a list of Tool
type ToolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Tool{}, &ToolList{})
}
