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

import "fmt"

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
// tool is meaningless without a backing MCPServer reference.
func (s *ToolSpec) Validate() error {
	if s.Type == "mcp" && s.MCPServerRef == nil {
		return fmt.Errorf("tool of type mcp requires spec.mcpServerRef")
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
	if dup := firstDuplicate(refNames(s.KnowledgeBaseRefs)); dup != "" {
		return fmt.Errorf("duplicate knowledgeBaseRef %q", dup)
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
