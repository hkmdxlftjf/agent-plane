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

// ToolPolicySpec governs, at fine granularity, which Tools an Agent may call
// and under what limits. Where Policy is coarse (classes of resources),
// ToolPolicy is tool-specific.
type ToolPolicySpec struct {
	// rules is an ordered list of per-tool rules. The first matching rule for a
	// given tool name applies.
	// +optional
	// +listType=atomic
	Rules []ToolRule `json:"rules,omitempty"`

	// defaultAction applies when no rule matches.
	// +kubebuilder:default=allow
	// +optional
	DefaultAction ToolAction `json:"defaultAction,omitempty"`
}

// ToolAction is the decision for a tool invocation.
// +kubebuilder:validation:Enum=allow;deny
type ToolAction string

const (
	ToolActionAllow ToolAction = "allow"
	ToolActionDeny  ToolAction = "deny"
)

// ToolRule matches one or more Tools and decides how they may be used.
type ToolRule struct {
	// tool matches a Tool by name. "*" matches any tool.
	// +kubebuilder:validation:MinLength=1
	// +required
	Tool string `json:"tool"`

	// action is allow or deny for the matched tool(s).
	// +required
	Action ToolAction `json:"action"`

	// maxCallsPerSession optionally caps invocations per agent session.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxCallsPerSession *int32 `json:"maxCallsPerSession,omitempty"`
}

// ToolPolicyStatus is the observed state of a ToolPolicy.
type ToolPolicyStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=tp
// +kubebuilder:printcolumn:name="Default",type=string,JSONPath=`.spec.defaultAction`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ToolPolicy is the Schema for the toolpolicies API.
type ToolPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of ToolPolicy
	// +required
	Spec ToolPolicySpec `json:"spec"`

	// status defines the observed state of ToolPolicy
	// +optional
	Status ToolPolicyStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ToolPolicyList contains a list of ToolPolicy
type ToolPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ToolPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ToolPolicy{}, &ToolPolicyList{})
}
