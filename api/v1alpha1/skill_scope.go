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
	"sort"
)

// SkillToolScope is one Skill's declared tool restriction, gathered by the
// caller from the Skills an Agent references.
type SkillToolScope struct {
	// Skill is the Skill CR's name.
	Skill string
	// AllowedTools is its spec.allowedTools.
	AllowedTools []string
}

// SkillToolViolations reports Skills whose allowedTools name a Tool the Agent
// cannot reach, as human-readable strings suitable for a status condition.
//
// This is an Agent-coherence check, not a policy decision: a skill body that
// instructs the model to use a tool the Agent never referenced is broken on its
// face, and the failure would otherwise surface only mid-conversation as a
// puzzling refusal. availableTools is the Agent's full tool surface (direct
// toolRefs plus everything its ToolSets contribute).
//
// A Skill declaring no allowedTools imposes nothing and is never reported.
func SkillToolViolations(skills []SkillToolScope, availableTools []string) []string {
	if len(skills) == 0 {
		return nil
	}
	available := make(map[string]bool, len(availableTools))
	for _, t := range availableTools {
		available[t] = true
	}
	var out []string
	for _, sk := range skills {
		for _, tool := range sk.AllowedTools {
			// "*" is not special here: allowedTools names concrete Tools, and a
			// literal "*" would be a Tool named "*".
			if !available[tool] {
				out = append(out, fmt.Sprintf(
					"skill %q allows tool %q, which the Agent does not reference", sk.Skill, tool))
			}
		}
	}
	sort.Strings(out)
	return out
}
