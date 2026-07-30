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
	"strings"
	"testing"
)

func TestSkillToolViolations(t *testing.T) {
	tests := []struct {
		name      string
		skills    []SkillToolScope
		available []string
		wantSubs  []string
		wantNone  bool
	}{
		{
			name:     "no skills",
			wantNone: true,
		},
		{
			// Most skills are pure instructions and restrict nothing.
			name:     "skill without allowedTools",
			skills:   []SkillToolScope{{Skill: "prose"}},
			wantNone: true,
		},
		{
			name:      "allowed tool the Agent references",
			skills:    []SkillToolScope{{Skill: skillRefunds, AllowedTools: []string{toolRefund}}},
			available: []string{toolRefund},
			wantNone:  true,
		},
		{
			// The case this check exists for: the skill body would send the model
			// after a tool that cannot be called.
			name:      "allowed tool the Agent does not reference",
			skills:    []SkillToolScope{{Skill: skillRefunds, AllowedTools: []string{toolRefund}}},
			available: []string{toolLookup},
			wantSubs:  []string{`skill "` + skillRefunds + `"`, wantToolRefundQuoted, "does not reference"},
		},
		{
			name:      "several unreachable tools are all reported",
			skills:    []SkillToolScope{{Skill: skillRefunds, AllowedTools: []string{toolRefund, "delete"}}},
			available: nil,
			wantSubs:  []string{wantToolRefundQuoted, `tool "delete"`},
		},
		{
			name: "only the unreachable half is reported",
			skills: []SkillToolScope{
				{Skill: "ok", AllowedTools: []string{toolLookup}},
				{Skill: "broken", AllowedTools: []string{toolRefund}},
			},
			available: []string{toolLookup},
			wantSubs:  []string{`skill "broken"`},
		},
		{
			// "*" is not a wildcard here: allowedTools names concrete Tools, so a
			// literal "*" must be reported rather than silently permitting anything.
			name:      "asterisk is not a wildcard",
			skills:    []SkillToolScope{{Skill: "sneaky", AllowedTools: []string{"*"}}},
			available: []string{toolRefund},
			wantSubs:  []string{`tool "*"`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SkillToolViolations(tc.skills, tc.available)
			if tc.wantNone {
				if len(got) != 0 {
					t.Fatalf("SkillToolViolations = %v, want none", got)
				}
				return
			}
			joined := strings.Join(got, "\n")
			for _, want := range tc.wantSubs {
				if !strings.Contains(joined, want) {
					t.Errorf("missing %q in:\n%s", want, joined)
				}
			}
		})
	}
}

// A skill listing the same unreachable tool as another must report both, so
// neither skill's problem is hidden by the other's.
func TestSkillToolViolationsReportsEachSkill(t *testing.T) {
	got := SkillToolViolations([]SkillToolScope{
		{Skill: "a", AllowedTools: []string{toolRefund}},
		{Skill: "b", AllowedTools: []string{toolRefund}},
	}, nil)
	if len(got) != 2 {
		t.Fatalf("SkillToolViolations = %v, want one entry per skill", got)
	}
}
