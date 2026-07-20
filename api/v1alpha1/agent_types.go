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

// AgentSpec describes the composition of an AI Agent. It is purely declarative:
// an Agent references the capabilities it is built from, and the Operator
// resolves those references into a single runtime configuration. The Agent
// itself says nothing about how the agent executes (that is the runtime's job).
type AgentSpec struct {
	// description is a human-readable summary of the agent's purpose.
	// +optional
	Description string `json:"description,omitempty"`

	// agentClassRef optionally references an AgentClass providing shared
	// defaults (base workflow, default policies) that this Agent inherits.
	// +optional
	AgentClassRef *LocalReference `json:"agentClassRef,omitempty"`

	// modelRef references the Model this agent uses. Optional at the API level so
	// it may be inherited from an AgentClass's defaultModelRef; an Agent with no
	// effective model (neither here nor via its class) is marked Degraded.
	// +optional
	ModelRef *LocalReference `json:"modelRef,omitempty"`

	// workflowRef references the Workflow describing the agent's execution
	// shape (e.g. planner/tool/reflect/finish). Optional when inherited from an
	// AgentClass or supplied by the runtime's default.
	// +optional
	WorkflowRef *LocalReference `json:"workflowRef,omitempty"`

	// promptRef references the PromptTemplate providing system/role prompts.
	// +optional
	PromptRef *LocalReference `json:"promptRef,omitempty"`

	// toolRefs lists individual Tools the agent may invoke.
	// +optional
	// +listType=atomic
	ToolRefs []LocalReference `json:"toolRefs,omitempty"`

	// toolSetRefs lists ToolSets (named bundles of Tools) the agent inherits.
	// +optional
	// +listType=atomic
	ToolSetRefs []LocalReference `json:"toolSetRefs,omitempty"`

	// skillRefs lists Skills (markdown instruction packs) the agent loads.
	// +optional
	// +listType=atomic
	SkillRefs []LocalReference `json:"skillRefs,omitempty"`

	// memoryRefs lists Memory backends available to the agent.
	// +optional
	// +listType=atomic
	MemoryRefs []LocalReference `json:"memoryRefs,omitempty"`

	// policyRefs lists Policies constraining the agent (model/memory/tool access).
	// +optional
	// +listType=atomic
	PolicyRefs []LocalReference `json:"policyRefs,omitempty"`

	// knowledgeBaseRefs lists KnowledgeBases the agent may retrieve from (RAG).
	// +optional
	// +listType=atomic
	KnowledgeBaseRefs []LocalReference `json:"knowledgeBaseRefs,omitempty"`

	// runtime, when set, tells the Operator to materialize an in-cluster agent
	// runtime Deployment for this Agent (pull model: the container reads its
	// config from the Registry). Omit to keep Agent Plane purely declarative and
	// bring your own runtime.
	// +optional
	Runtime *AgentRuntimeSpec `json:"runtime,omitempty"`
}

// ApplyClassDefaults returns a copy of spec with unset optional references
// filled from the referenced AgentClass. Agent-level values always win; class
// defaults only fill gaps. It is the single source of the effective spec, used
// by both the Operator (resolution + hash) and the Registry (data-plane config)
// so the two never diverge. class may be nil (returns spec unchanged).
func ApplyClassDefaults(spec AgentSpec, class *AgentClass) AgentSpec {
	if class == nil {
		return spec
	}
	if spec.ModelRef == nil && class.Spec.DefaultModelRef != nil {
		spec.ModelRef = class.Spec.DefaultModelRef
	}
	if spec.WorkflowRef == nil && class.Spec.DefaultWorkflowRef != nil {
		spec.WorkflowRef = class.Spec.DefaultWorkflowRef
	}
	if spec.PromptRef == nil && class.Spec.DefaultPromptRef != nil {
		spec.PromptRef = class.Spec.DefaultPromptRef
	}
	if len(spec.PolicyRefs) == 0 && len(class.Spec.DefaultPolicyRefs) > 0 {
		spec.PolicyRefs = class.Spec.DefaultPolicyRefs
	}
	return spec
}

// AgentRuntimeSpec describes an operator-managed runtime workload for an Agent.
// The runtime image is user-supplied; Agent Plane only schedules it and wires it to
// the Registry (it does not do inference itself).
type AgentRuntimeSpec struct {
	// image is the agent runtime container image to run.
	// +kubebuilder:validation:MinLength=1
	// +required
	Image string `json:"image"`

	// replicas is the desired number of runtime pods.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// port, if set, exposes the runtime via a Service on this port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// env is extra environment passed to the runtime container. The Operator
	// always injects AGENTPLANE_REGISTRY, AGENTPLANE_AGENT_NAMESPACE, AGENTPLANE_AGENT_NAME.
	// +optional
	// +listType=atomic
	Env []corev1.EnvVar `json:"env,omitempty"`

	// resources is the runtime container resource requirements.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// AgentPhase is a coarse, human-facing summary of an Agent's reconcile state.
// +kubebuilder:validation:Enum=Pending;Ready;Degraded
type AgentPhase string

const (
	AgentPhasePending  AgentPhase = "Pending"
	AgentPhaseReady    AgentPhase = "Ready"
	AgentPhaseDegraded AgentPhase = "Degraded"
)

// AgentStatus is the observed state of an Agent.
type AgentStatus struct {
	// conditions represent the current state of the Agent resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// phase is a coarse summary of reconcile state.
	// +optional
	Phase AgentPhase `json:"phase,omitempty"`

	// resolvedConfigHash is a stable hash of the fully resolved runtime
	// configuration (model + workflow + tools + prompt + memory + policies).
	// Runtimes and the Registry use it to detect when config must be refreshed.
	// +optional
	ResolvedConfigHash string `json:"resolvedConfigHash,omitempty"`

	// resolvedRefs is the number of references that resolved successfully.
	// +optional
	ResolvedRefs int `json:"resolvedRefs,omitempty"`

	// runtimeAvailableReplicas mirrors the managed runtime Deployment's
	// available replicas (only when spec.runtime is set).
	// +optional
	RuntimeAvailableReplicas int32 `json:"runtimeAvailableReplicas,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ag
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.modelRef.name`
// +kubebuilder:printcolumn:name="Hash",type=string,JSONPath=`.status.resolvedConfigHash`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Agent is the Schema for the agents API.
type Agent struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Agent
	// +required
	Spec AgentSpec `json:"spec"`

	// status defines the observed state of Agent
	// +optional
	Status AgentStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// AgentList contains a list of Agent
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Agent{}, &AgentList{})
}
