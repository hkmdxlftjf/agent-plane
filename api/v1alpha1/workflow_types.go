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

// WorkflowSpec describes the execution shape of an agent (e.g. planner → tool →
// reflect → finish). It is engine-neutral: the same Workflow can be realized by
// LangGraph, an Agents SDK, CrewAI, etc. Agent Plane only stores the declaration.
type WorkflowSpec struct {
	// engine names the target runtime engine this workflow is authored for
	// (e.g. "langgraph", "openai-agents", "crewai"). Free-form by design so new
	// engines need no platform change.
	// +optional
	Engine string `json:"engine,omitempty"`

	// version is the semantic version of the workflow definition.
	// +optional
	Version string `json:"version,omitempty"`

	// steps is an ordered list of workflow steps. The interpretation of `next`
	// is engine-specific; Agent Plane does not execute the graph.
	// +optional
	// +listType=atomic
	Steps []WorkflowStep `json:"steps,omitempty"`
}

// WorkflowStep is a single node in a workflow.
type WorkflowStep struct {
	// name uniquely identifies the step within the workflow.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// type categorizes the step (e.g. "planner", "tool", "reflect", "finish").
	// +optional
	Type string `json:"type,omitempty"`

	// next lists the names of steps that may follow this one.
	// +optional
	// +listType=atomic
	Next []string `json:"next,omitempty"`
}

// WorkflowStatus is the observed state of a Workflow.
type WorkflowStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=wf
// +kubebuilder:printcolumn:name="Engine",type=string,JSONPath=`.spec.engine`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Workflow is the Schema for the workflows API.
type Workflow struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Workflow
	// +required
	Spec WorkflowSpec `json:"spec"`

	// status defines the observed state of Workflow
	// +optional
	Status WorkflowStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// WorkflowList contains a list of Workflow
type WorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workflow `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Workflow{}, &WorkflowList{})
}
