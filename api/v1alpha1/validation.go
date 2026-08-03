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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// This file centralizes *structural* validation — invariants that are always
// wrong regardless of what else exists in the cluster (duplicate names, dangling
// references within a single object, contradictory rules). Both the controllers
// (defense in depth, and to surface problems in status) and the admission
// webhooks (fail fast at apply time) call these.
//
// Cross-object existence checks (does the referenced Model exist?) are NOT here:
// those are the controllers' job via eventual consistency, so that GitOps can
// apply resources in any order without admission rejecting them.

// Validate reports the first structural problem in a WorkflowSpec, or nil.
// Step names must be unique and every `next` target must name a declared step.
func (s *WorkflowSpec) Validate() error {
	names := make(map[string]bool, len(s.Steps))
	for _, step := range s.Steps {
		if names[step.Name] {
			return fmt.Errorf("duplicate step name %q", step.Name)
		}
		names[step.Name] = true
	}
	for _, step := range s.Steps {
		for _, next := range step.Next {
			if !names[next] {
				return fmt.Errorf("step %q references unknown next step %q", step.Name, next)
			}
		}
	}
	return nil
}

// Validate reports the first structural problem in a ToolPolicySpec, or nil.
// A specific tool (not the "*" wildcard) may not appear with two different
// actions.
func (s *ToolPolicySpec) Validate() error {
	seen := make(map[string]ToolAction, len(s.Rules))
	for _, rule := range s.Rules {
		if rule.Tool == "*" {
			continue
		}
		if prev, ok := seen[rule.Tool]; ok && prev != rule.Action {
			return fmt.Errorf("tool %q has conflicting actions %q and %q", rule.Tool, prev, rule.Action)
		}
		seen[rule.Tool] = rule.Action
	}
	return nil
}

// Validate reports the first structural problem in a ToolSpec, or nil. An mcp
// tool is meaningless without a backing MCPServer reference — or, for a tool
// that consults another Agent, an agentRef. Exactly one of the two applies.
func (s *ToolSpec) Validate() error {
	if s.MCPServerRef != nil && s.AgentRef != nil {
		return fmt.Errorf("set only one of spec.mcpServerRef or spec.agentRef, not both")
	}
	if s.Type == ToolTypeMCP && s.MCPServerRef == nil && s.AgentRef == nil {
		return fmt.Errorf("tool of type mcp requires spec.mcpServerRef or spec.agentRef")
	}
	if s.AgentRef != nil && s.Type != ToolTypeMCP {
		return fmt.Errorf("spec.agentRef requires type mcp, got %q", s.Type)
	}
	return nil
}

// Validate reports the first structural problem in an AgentSpec, or nil. The
// same capability must not be referenced twice within a single list.
func (s *AgentSpec) Validate() error {
	if dup := firstDuplicate(refNames(s.ToolRefs)); dup != "" {
		return fmt.Errorf("duplicate toolRef %q", dup)
	}
	if dup := firstDuplicate(refNames(s.ToolSetRefs)); dup != "" {
		return fmt.Errorf("duplicate toolSetRef %q", dup)
	}
	if dup := firstDuplicate(refNames(s.SkillRefs)); dup != "" {
		return fmt.Errorf("duplicate skillRef %q", dup)
	}
	if dup := firstDuplicate(refNames(s.MemoryRefs)); dup != "" {
		return fmt.Errorf("duplicate memoryRef %q", dup)
	}
	if dup := firstDuplicate(refNames(s.PolicyRefs)); dup != "" {
		return fmt.Errorf("duplicate policyRef %q", dup)
	}
	if dup := firstDuplicate(refNames(s.ToolPolicyRefs)); dup != "" {
		return fmt.Errorf("duplicate toolPolicyRef %q", dup)
	}
	if dup := firstDuplicate(refNames(s.KnowledgeBaseRefs)); dup != "" {
		return fmt.Errorf("duplicate knowledgeBaseRef %q", dup)
	}
	if err := s.validateWorkspace(); err != nil {
		return err
	}
	if err := s.validateExpose(); err != nil {
		return err
	}
	return nil
}

// validateWorkspace checks the repository binding. A workspace needs somewhere
// to live, so it depends on spec.runtime — the Operator mounts the working tree
// into the runtime pod, and without one there is no pod to mount it into.
func (s *AgentSpec) validateWorkspace() error {
	ws := s.Workspace
	if ws == nil {
		return nil
	}
	if s.Runtime == nil {
		return fmt.Errorf("spec.workspace requires spec.runtime: the working tree is mounted into the runtime pod")
	}
	if ws.MountPath != "" && !strings.HasPrefix(ws.MountPath, "/") {
		return fmt.Errorf("spec.workspace.mountPath %q must be absolute", ws.MountPath)
	}
	if ws.Size != "" {
		if _, err := resource.ParseQuantity(ws.Size); err != nil {
			return fmt.Errorf("spec.workspace.size %q is not a valid quantity: %w", ws.Size, err)
		}
	}
	return nil
}

// validateExpose checks the peer endpoint. Exposing an Agent means publishing
// its runtime, so it likewise depends on spec.runtime.
func (s *AgentSpec) validateExpose() error {
	if s.Expose == nil {
		return nil
	}
	if s.Runtime == nil {
		return fmt.Errorf("spec.expose requires spec.runtime: there is no runtime to publish")
	}
	return nil
}

// Validate reports the first structural problem in a SkillSpec, or nil. Exactly
// one content source (inline body or ConfigMap reference) must be set.
func (s *SkillSpec) Validate() error {
	hasInline := s.Content != ""
	hasRef := s.ContentConfigMapRef != nil
	switch {
	case hasInline && hasRef:
		return fmt.Errorf("set only one of spec.content or spec.contentConfigMapRef, not both")
	case !hasInline && !hasRef:
		return fmt.Errorf("one of spec.content or spec.contentConfigMapRef is required")
	}
	return nil
}

// Contract environment variable names injected into an adapter container. They
// are exported because both the Operator (which sets them) and validation
// (which forbids a Trigger from setting them) must agree, and an adapter author
// reading the Go docs should see the exact strings.
const (
	EnvAgentEndpoint  = "AGENTPLANE_AGENT_ENDPOINT"
	EnvCredentialPath = "AGENTPLANE_CREDENTIAL_PATH"
)

// ReservedTriggerEnv are the environment variables the Operator injects into an
// adapter container. They are the adapter contract, so a Trigger may not set
// them itself — silently overriding AGENTPLANE_AGENT_ENDPOINT would point the
// adapter at the wrong Agent, which is far easier to debug at apply time than at
// runtime.
var ReservedTriggerEnv = []string{
	EnvAgentEndpoint,
	"AGENTPLANE_AGENT_NAME",
	"AGENTPLANE_AGENT_NAMESPACE",
	"AGENTPLANE_TRIGGER_NAME",
	"AGENTPLANE_TRIGGER_CONFIG",
	EnvCredentialPath,
}

// Validate reports the first structural problem in a TriggerSpec, or nil.
func (s *TriggerSpec) Validate() error {
	if s.AgentRef.Name == "" {
		return fmt.Errorf("spec.agentRef.name is required")
	}
	reserved := make(map[string]bool, len(ReservedTriggerEnv))
	for _, name := range ReservedTriggerEnv {
		reserved[name] = true
	}
	for _, e := range s.Env {
		if reserved[e.Name] {
			return fmt.Errorf("spec.env may not set %q: it is injected by the Operator", e.Name)
		}
	}
	if dup := firstDuplicate(envNames(s.Env)); dup != "" {
		return fmt.Errorf("duplicate env var %q", dup)
	}
	return nil
}

func envNames(env []corev1.EnvVar) []string {
	names := make([]string, len(env))
	for i, e := range env {
		names[i] = e.Name
	}
	return names
}

func refNames(refs []LocalReference) []string {
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.Name
	}
	return names
}

func firstDuplicate(names []string) string {
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if seen[n] {
			return n
		}
		seen[n] = true
	}
	return ""
}
