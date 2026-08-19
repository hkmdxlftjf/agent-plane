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
	"k8s.io/apimachinery/pkg/api/resource"
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

	// toolPolicyRefs lists ToolPolicies governing individual tool calls
	// (per-tool allow/deny and per-session call caps). Where policyRefs is
	// coarse, these are tool-specific; both are enforced by the runtime, which
	// receives the merged result from the Registry.
	// +optional
	// +listType=atomic
	ToolPolicyRefs []LocalReference `json:"toolPolicyRefs,omitempty"`

	// knowledgeBaseRefs lists KnowledgeBases the agent may retrieve from (RAG).
	// +optional
	// +listType=atomic
	KnowledgeBaseRefs []LocalReference `json:"knowledgeBaseRefs,omitempty"`

	// credentialRefs lists Credentials the runtime needs for things Agent Plane
	// does not model — an IM app secret, a home automation token, a vendor API
	// key. Each one's Secret is mounted as files under
	// $AGENTPLANE_CREDENTIALS_PATH/<credential-name>/, one file per key.
	//
	// This exists because the alternative was spec.runtime.env with a
	// secretKeyRef, and the two are not equivalent. An environment value is
	// visible in `kubectl describe pod`, is inherited by every child process the
	// agent spawns, and tends to end up in logs and crash dumps. A file is none
	// of those.
	//
	// What it does NOT do is hide the value from the model. A runtime that
	// executes model-directed shell commands can read the file, exactly as it
	// could read the environment. Mounting narrows where the secret leaks by
	// accident; it is not a boundary against the agent itself. An Agent that must
	// not hold a credential should reach the capability through a Tool whose MCP
	// server holds it instead.
	//
	// A missing Credential leaves the Agent Degraded with ReferenceNotFound, the
	// same as any other unresolved reference.
	// +optional
	// +listType=atomic
	CredentialRefs []LocalReference `json:"credentialRefs,omitempty"`

	// runtime, when set, tells the Operator to materialize an in-cluster agent
	// runtime Deployment for this Agent (pull model: the container reads its
	// config from the Registry). Omit to keep Agent Plane purely declarative and
	// bring your own runtime.
	// +optional
	Runtime *AgentRuntimeSpec `json:"runtime,omitempty"`

	// workspace gives this Agent a persistent working directory: a volume the
	// Operator provisions and the runtime pod mounts, so whatever the agent
	// accumulates — files it writes, caches it builds, the conversation history a
	// runtime keeps under HOME — survives a restart.
	//
	// Set repository to populate it from git, which is what a coding agent
	// (Claude Code, Codex, OpenCode, …) needs. Leave it unset and the directory
	// starts empty: an assistant that files notes, caches what it fetched, or
	// simply needs somewhere writable gets the same durable volume, and the same
	// sandbox around it, without pretending to own a repository.
	//
	// Either way the volume has exactly one writer, so the runtime Deployment is
	// pinned to a single replica. For a repository-bound Agent, cross-repository
	// work is expressed by referencing another Agent as a Tool rather than by
	// mounting a second repo: see spec.expose. That keeps each working tree
	// single-writer, and makes the dependency between repositories a declared,
	// policeable edge instead of a shared mount.
	// +optional
	Workspace *AgentWorkspaceSpec `json:"workspace,omitempty"`

	// expose, when set, publishes this Agent's runtime as an MCP endpoint so
	// other Agents can consult it. Peers reach it the ordinary way — a Tool of
	// type mcp naming this Agent — which means Policy and ToolPolicy govern
	// agent-to-agent traffic with no separate authorization model.
	// +optional
	Expose *AgentExposeSpec `json:"expose,omitempty"`
}

// AgentWorkspaceSpec is the Agent's persistent working directory, optionally
// populated from a git repository.
type AgentWorkspaceSpec struct {
	// repository is the clone URL (https or ssh) to populate the working
	// directory from. Omit it for a working directory that starts empty.
	//
	// branch and credentialRef only mean anything alongside it — there is
	// nothing to check out or authenticate against without a repository, so
	// setting either one alone is rejected rather than silently ignored.
	// +optional
	Repository string `json:"repository,omitempty"`

	// branch to check out. Defaults to the repository's default branch.
	// +optional
	Branch string `json:"branch,omitempty"`

	// credentialRef references a Credential whose Secret holds the git
	// credential (a token, or an SSH key). It is mounted into the clone step,
	// never passed as an environment value.
	// +optional
	CredentialRef *LocalReference `json:"credentialRef,omitempty"`

	// mountPath is where the working tree appears in the runtime container.
	// +kubebuilder:default=/workspace
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// size is the persistent volume request for the working tree.
	// +kubebuilder:default="10Gi"
	// +optional
	Size string `json:"size,omitempty"`

	// storageClassName selects the StorageClass for the working tree. Omit to
	// use the cluster default.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// AgentExposeSpec publishes an Agent's runtime as an MCP endpoint for peers.
type AgentExposeSpec struct {
	// port the runtime serves MCP on. Must match what the runtime image
	// listens on; the Operator only publishes it.
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// description tells a peer Agent's model what this Agent is good for — it
	// becomes the tool description on the other side, so write it for a model
	// deciding whether to ask ("Owns the payments API; answers questions about
	// its schema and endpoints"), not as an internal note.
	// +optional
	Description string `json:"description,omitempty"`
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
	if len(spec.ToolPolicyRefs) == 0 && len(class.Spec.DefaultToolPolicyRefs) > 0 {
		spec.ToolPolicyRefs = class.Spec.DefaultToolPolicyRefs
	}
	if len(spec.ToolRefs) == 0 && len(class.Spec.DefaultToolRefs) > 0 {
		spec.ToolRefs = class.Spec.DefaultToolRefs
	}
	if len(spec.SkillRefs) == 0 && len(class.Spec.DefaultSkillRefs) > 0 {
		spec.SkillRefs = class.Spec.DefaultSkillRefs
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

	// serviceAccountName is the ServiceAccount the runtime pods run as.
	//
	// A runtime needs one whenever its Model carries a credentialRef: the Registry
	// serves Secret *coordinates* and the runtime reads the value itself, which
	// requires get on secrets in this namespace. Left empty the pods run as the
	// namespace's "default" ServiceAccount, and the only way to grant that read is
	// to bind the permission to "default" — which grants it to every other pod in
	// the namespace too. Naming a dedicated account here keeps the grant scoped to
	// this runtime.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// resources is the runtime container resource requirements.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// readinessProbe overrides how readiness is decided for this runtime.
	//
	// Set it when "the process is listening" and "the agent can answer" are not
	// the same thing — which is the common case for a runtime that does work at
	// startup. A runtime that fetches config, resolves plugins, or opens an
	// upstream connection can bind its port, answer a trivial health endpoint,
	// and still hang on every real request; without a probe that distinguishes
	// the two, Kubernetes reports 1/1 Running and the failure is invisible.
	//
	// Left unset, a workspace-bound runtime gets a default probe (see
	// defaultWorkspaceReadinessProbe in internal/controller) and any other
	// runtime gets none — an Agent that only serves /api/chat is ready as soon as
	// it listens.
	// +optional
	ReadinessProbe *corev1.Probe `json:"readinessProbe,omitempty"`

	// runtimeClassName selects a container runtime for the pod, e.g. "gvisor"
	// for a syscall-intercepting sandbox.
	//
	// Worth setting when the Agent runs code you do not trust: a coding agent
	// executes model-directed shell commands, and the container-level sandbox the
	// Operator applies (read-only root filesystem, no capabilities, non-root)
	// is enforced by the host kernel. A sandboxed runtime raises the cost of a
	// kernel escape, at some latency. The named RuntimeClass must already exist
	// in the cluster; naming one that does not leaves pods Pending.
	// +optional
	RuntimeClassName string `json:"runtimeClassName,omitempty"`

	// volumes are extra volumes mounted into the runtime container: a NAS share
	// through its PersistentVolumeClaim, a ConfigMap of settings, a Secret of
	// API keys, a scratch directory.
	//
	// This is what lets an Agent reach something the control plane knows nothing
	// about, without a new field per storage kind. spec.workspace remains the
	// agent's own working directory; these are everything else.
	//
	// The volume sources are a deliberate subset of the pod API rather than all
	// of it, and hostPath is the notable omission: a workspace runtime runs
	// model-directed shell commands under a read-only root filesystem, dropped
	// capabilities and a non-root uid, and one hostPath mount would step around
	// all of it. An external filesystem belongs behind a PersistentVolumeClaim,
	// which is also where its credentials stay.
	//
	// A volume may not reuse a name or a mount path the Operator already uses
	// (workspace, tmp, git-credential, model-credential, and the credential
	// directory) — that is rejected at apply time rather than resolved silently,
	// since a mount that shadows one of those fails in ways that look like
	// anything but a name collision.
	// +optional
	// +listType=map
	// +listMapKey=name
	Volumes []AgentVolume `json:"volumes,omitempty"`
}

// AgentVolume is one extra volume mounted into the runtime container, declared
// as the volume and its mount together.
//
// Kubernetes splits these into two lists matched by name; keeping them as one
// entry removes the failure that split invites — a volume with no mount, or a
// mount naming a volume that is not there. An Agent volume goes into exactly one
// container, so there is nothing the split would buy.
//
// Exactly one source must be set. The set is deliberately smaller than the pod
// API's: see spec.runtime.volumes for why hostPath is not in it.
// +kubebuilder:validation:XValidation:rule="[has(self.persistentVolumeClaim), has(self.configMap), has(self.secret), has(self.emptyDir)].exists_one(x, x)",message="exactly one of persistentVolumeClaim, configMap, secret or emptyDir must be set"
type AgentVolume struct {
	// name identifies the volume within the pod. Must not collide with a volume
	// the Operator manages.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +required
	Name string `json:"name"`

	// mountPath is the absolute path the volume appears at in the runtime
	// container.
	// +kubebuilder:validation:MinLength=1
	// +required
	MountPath string `json:"mountPath"`

	// readOnly mounts the volume read-only.
	//
	// Worth setting for anything the agent only needs to read. The runtime may be
	// executing shell commands a model chose, and a read-only mount is enforced
	// by the kernel rather than by the agent deciding to behave.
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`

	// persistentVolumeClaim mounts an existing PVC by name. This is how an
	// external filesystem — NFS, CIFS, a NAS share, a cloud disk — reaches the
	// agent: bind it to a PVC with the cluster's usual storage machinery, and any
	// credentials it needs stay in the PersistentVolume rather than in this
	// resource.
	// +optional
	PersistentVolumeClaim *AgentPVCVolumeSource `json:"persistentVolumeClaim,omitempty"`

	// configMap mounts a ConfigMap's keys as files.
	// +optional
	ConfigMap *AgentObjectVolumeSource `json:"configMap,omitempty"`

	// secret mounts a Secret's keys as files.
	//
	// Note what this does and does not do: it keeps the value out of the pod's
	// environment and out of `kubectl describe pod`, but a runtime that executes
	// model-directed shell commands can still read the file. See
	// spec.credentialRefs, which is the same mechanism with the Credential
	// indirection the rest of Agent Plane uses.
	// +optional
	Secret *AgentObjectVolumeSource `json:"secret,omitempty"`

	// emptyDir is scratch space that lives as long as the pod. Useful under a
	// read-only root filesystem, where a build or a download needs somewhere to
	// land that is not the working directory.
	// +optional
	EmptyDir *AgentEmptyDirVolumeSource `json:"emptyDir,omitempty"`
}

// AgentPVCVolumeSource names an existing PersistentVolumeClaim.
type AgentPVCVolumeSource struct {
	// claimName is the PersistentVolumeClaim in this namespace. It must already
	// exist — the Operator provisions a claim for spec.workspace, but never for
	// one declared here.
	// +kubebuilder:validation:MinLength=1
	// +required
	ClaimName string `json:"claimName"`
}

// AgentObjectVolumeSource names a ConfigMap or Secret to mount as files.
type AgentObjectVolumeSource struct {
	// name is the ConfigMap or Secret in this namespace.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// optional lets the pod start when the object does not exist yet. Left false,
	// the pod stays Pending until it appears, which is usually what you want:
	// starting an agent without the configuration it was given is rarely better
	// than waiting for it.
	// +optional
	Optional *bool `json:"optional,omitempty"`
}

// AgentEmptyDirVolumeSource is pod-lifetime scratch space.
type AgentEmptyDirVolumeSource struct {
	// sizeLimit caps the volume, e.g. "1Gi". Worth setting: an unbounded
	// emptyDir on the node's filesystem is evicted only once the node is already
	// under disk pressure.
	// +optional
	SizeLimit *resource.Quantity `json:"sizeLimit,omitempty"`

	// medium set to "Memory" backs the volume with a tmpfs, which counts against
	// the container's memory limit.
	// +kubebuilder:validation:Enum="";Memory
	// +optional
	Medium corev1.StorageMedium `json:"medium,omitempty"`
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

	// peerEndpoint is the in-cluster MCP address other Agents use to consult
	// this one (only when spec.expose is set). Published here so a peer's Tool
	// resolves the same way an MCPServer-backed Tool does.
	// +optional
	PeerEndpoint string `json:"peerEndpoint,omitempty"`

	// workspaceClaim names the PersistentVolumeClaim holding this Agent's
	// working tree (only when spec.workspace is set).
	// +optional
	WorkspaceClaim string `json:"workspaceClaim,omitempty"`
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
