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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func policyNamed(name string, spec PolicySpec) Policy {
	return Policy{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
}

func toolPolicyNamed(name string, spec ToolPolicySpec) ToolPolicy {
	return ToolPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
}

func i32(v int32) *int32 { return &v }

// Fixture literals, extracted so the repetition inherent to table tests does
// not read as a set of unrelated magic strings.
const (
	modelClaude = "claude"
	modelGPT4   = "gpt-4"
	toolRefund  = "refund"
	toolLookup  = "lookup"

	// Expected fragments of violation messages, shared by the policy and skill
	// scope tests.
	wantToolRefundQuoted = `tool "refund"`
	skillRefunds         = "refunds"
)

// No policies means nothing to enforce, and callers rely on nil to skip the
// check entirely.
func TestMergePoliciesEmpty(t *testing.T) {
	if got := MergePolicies(nil, nil); got != nil {
		t.Errorf("MergePolicies(nil, nil) = %+v, want nil", got)
	}
}

func TestMergePoliciesRecordsSources(t *testing.T) {
	eff := MergePolicies(
		[]Policy{policyNamed("guardrails", PolicySpec{}), policyNamed("tenant", PolicySpec{})},
		[]ToolPolicy{toolPolicyNamed("tools", ToolPolicySpec{})},
	)
	want := []string{"Policy/guardrails", "Policy/tenant", "ToolPolicy/tools"}
	if strings.Join(eff.Sources, ",") != strings.Join(want, ",") {
		t.Errorf("Sources = %v, want %v", eff.Sources, want)
	}
}

// Merging must only ever narrow: allow lists intersect, deny lists union. This
// is the property that makes "attach another Policy" a safe operation.
func TestMergePoliciesOnlyNarrows(t *testing.T) {
	eff := MergePolicies([]Policy{
		policyNamed("a", PolicySpec{Models: &AccessRule{Allow: []string{modelClaude, modelGPT4}}}),
		policyNamed("b", PolicySpec{Models: &AccessRule{Allow: []string{modelClaude, "llama"}}}),
	}, nil)
	if got := strings.Join(eff.Models.Allow, ","); got != modelClaude {
		t.Errorf("merged allow = %q, want the intersection %q", got, modelClaude)
	}

	eff = MergePolicies([]Policy{
		policyNamed("a", PolicySpec{Tools: &AccessRule{Deny: []string{toolRefund}}}),
		policyNamed("b", PolicySpec{Tools: &AccessRule{Deny: []string{"delete"}}}),
	}, nil)
	if len(eff.Tools.Deny) != 2 {
		t.Errorf("merged deny = %v, want the union of both", eff.Tools.Deny)
	}
}

// An empty allow list means "unconstrained", so merging one in must not collapse
// the other side to the empty set — that would deny everything by accident.
func TestMergeEmptyAllowIsNotAnEmptySet(t *testing.T) {
	eff := MergePolicies([]Policy{
		policyNamed("a", PolicySpec{Models: &AccessRule{Allow: []string{modelClaude}}}),
		policyNamed("b", PolicySpec{Models: &AccessRule{Deny: []string{modelGPT4}}}),
	}, nil)
	if !eff.Models.Permits(modelClaude) {
		t.Error("claude should still be permitted after merging a deny-only Policy")
	}
	if eff.Models.Permits(modelGPT4) {
		t.Error("gpt-4 should be denied")
	}
}

// A wildcard allow on one side must not narrow the other side's list.
func TestMergeWildcardAllow(t *testing.T) {
	eff := MergePolicies([]Policy{
		policyNamed("a", PolicySpec{Models: &AccessRule{Allow: []string{modelClaude}}}),
		policyNamed("b", PolicySpec{Models: &AccessRule{Allow: []string{"*"}}}),
	}, nil)
	if !eff.Models.Permits(modelClaude) {
		t.Error("claude should survive a wildcard allow on the other side")
	}
}

// Deny wins across CRs: one Policy allowing a model cannot rescue it from
// another Policy's deny.
func TestMergeDenyBeatsAllowAcrossPolicies(t *testing.T) {
	eff := MergePolicies([]Policy{
		policyNamed("permissive", PolicySpec{Models: &AccessRule{Allow: []string{modelGPT4}}}),
		policyNamed("restrictive", PolicySpec{Models: &AccessRule{Deny: []string{modelGPT4}}}),
	}, nil)
	if eff.Models.Permits(modelGPT4) {
		t.Error("a deny in any Policy must win over an allow in another")
	}
}

func TestMergeDefaultToolActionDenyWins(t *testing.T) {
	eff := MergePolicies(nil, []ToolPolicy{
		toolPolicyNamed("a", ToolPolicySpec{DefaultAction: ToolActionAllow}),
		toolPolicyNamed("b", ToolPolicySpec{DefaultAction: ToolActionDeny}),
	})
	if eff.DefaultToolAction != ToolActionDeny {
		t.Errorf("DefaultToolAction = %q, want deny", eff.DefaultToolAction)
	}
}

// An unset defaultAction is the CRD default (allow) and must not be read as
// deny by the zero value.
func TestMergeUnsetDefaultActionIsAllow(t *testing.T) {
	eff := MergePolicies(nil, []ToolPolicy{toolPolicyNamed("a", ToolPolicySpec{})})
	if eff.DefaultToolAction != ToolActionAllow {
		t.Errorf("DefaultToolAction = %q, want allow", eff.DefaultToolAction)
	}
}

func TestAccessRulePermits(t *testing.T) {
	tests := []struct {
		name  string
		rule  *AccessRule
		probe string
		want  bool
	}{
		{"nil permits", nil, "x", true},
		{"empty permits", &AccessRule{}, "x", true},
		{"deny blocks", &AccessRule{Deny: []string{"x"}}, "x", false},
		{"deny wildcard blocks all", &AccessRule{Deny: []string{"*"}}, "x", false},
		{"allow is exhaustive", &AccessRule{Allow: []string{"y"}}, "x", false},
		{"allow listed passes", &AccessRule{Allow: []string{"x"}}, "x", true},
		{"allow wildcard passes", &AccessRule{Allow: []string{"*"}}, "x", true},
		{"deny beats allow", &AccessRule{Allow: []string{"x"}, Deny: []string{"x"}}, "x", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.Permits(tc.probe); got != tc.want {
				t.Errorf("Permits(%q) = %v, want %v", tc.probe, got, tc.want)
			}
		})
	}
}

func TestMatchToolRule(t *testing.T) {
	rules := []ToolRule{
		{Tool: "*", Action: ToolActionDeny},
		{Tool: toolLookup, Action: ToolActionAllow},
	}
	if got := MatchToolRule(rules, toolLookup); got == nil || got.Action != ToolActionAllow {
		t.Errorf("exact match should beat an earlier wildcard, got %+v", got)
	}
	if got := MatchToolRule(rules, "other"); got == nil || got.Action != ToolActionDeny {
		t.Errorf("unmatched name should fall to the wildcard, got %+v", got)
	}
	if got := MatchToolRule(nil, "x"); got != nil {
		t.Errorf("MatchToolRule(nil) = %+v, want nil", got)
	}
}

func TestViolations(t *testing.T) {
	tests := []struct {
		name     string
		eff      *EffectivePolicy
		refs     AgentReferences
		wantSubs []string // substrings expected in the violation list
		wantNone bool
	}{
		{
			name:     "nil policy never violates",
			eff:      nil,
			refs:     AgentReferences{Model: "anything", Tools: []string{toolRefund}},
			wantNone: true,
		},
		{
			name:     "denied model",
			eff:      MergePolicies([]Policy{policyNamed("p", PolicySpec{Models: &AccessRule{Deny: []string{modelGPT4}}})}, nil),
			refs:     AgentReferences{Model: modelGPT4},
			wantSubs: []string{`model "gpt-4" is denied`},
		},
		{
			name:     "allowed model passes",
			eff:      MergePolicies([]Policy{policyNamed("p", PolicySpec{Models: &AccessRule{Allow: []string{modelClaude}}})}, nil),
			refs:     AgentReferences{Model: modelClaude},
			wantNone: true,
		},
		{
			name:     "denied memory",
			eff:      MergePolicies([]Policy{policyNamed("p", PolicySpec{Memory: &AccessRule{Allow: []string{"redis"}}})}, nil),
			refs:     AgentReferences{Memories: []string{"postgres"}},
			wantSubs: []string{`memory "postgres" is denied`},
		},
		{
			name:     "denied mcp server reached via a tool",
			eff:      MergePolicies([]Policy{policyNamed("p", PolicySpec{MCP: &AccessRule{Deny: []string{"amap"}}})}, nil),
			refs:     AgentReferences{MCPServers: []string{"amap"}},
			wantSubs: []string{`mcpServer "amap" is denied`},
		},
		{
			name:     "denied workflow",
			eff:      MergePolicies([]Policy{policyNamed("p", PolicySpec{Workflows: &AccessRule{Deny: []string{"planner"}}})}, nil),
			refs:     AgentReferences{Workflow: "planner"},
			wantSubs: []string{`workflow "planner" is denied`},
		},
		{
			name: "tool denied by a ToolPolicy rule",
			eff: MergePolicies(nil, []ToolPolicy{toolPolicyNamed("tp", ToolPolicySpec{
				Rules: []ToolRule{{Tool: toolRefund, Action: ToolActionDeny}},
			})}),
			refs:     AgentReferences{Tools: []string{toolRefund}},
			wantSubs: []string{wantToolRefundQuoted + " is denied by a ToolPolicy rule"},
		},
		{
			name: "tool unreachable under default deny",
			eff: MergePolicies(nil, []ToolPolicy{toolPolicyNamed("tp", ToolPolicySpec{
				Rules:         []ToolRule{{Tool: toolLookup, Action: ToolActionAllow}},
				DefaultAction: ToolActionDeny,
			})}),
			refs:     AgentReferences{Tools: []string{toolRefund}},
			wantSubs: []string{"matches no ToolPolicy rule"},
		},
		{
			name: "an explicitly allowed tool passes under default deny",
			eff: MergePolicies(nil, []ToolPolicy{toolPolicyNamed("tp", ToolPolicySpec{
				Rules:         []ToolRule{{Tool: toolLookup, Action: ToolActionAllow}},
				DefaultAction: ToolActionDeny,
			})}),
			refs:     AgentReferences{Tools: []string{toolLookup}},
			wantNone: true,
		},
		{
			// A cap is a call-time concern: declaring a capped tool is legitimate,
			// so the Agent must still reach Ready. Only the runtime can count calls.
			name: "a capped tool is not a declaration violation",
			eff: MergePolicies(nil, []ToolPolicy{toolPolicyNamed("tp", ToolPolicySpec{
				Rules: []ToolRule{{Tool: toolRefund, Action: ToolActionAllow, MaxCallsPerSession: i32(1)}},
			})}),
			refs:     AgentReferences{Tools: []string{toolRefund}},
			wantNone: true,
		},
		{
			// maxCallsPerSession: 0 means the tool can never be called, which is a
			// contradiction worth surfacing at apply time rather than at call time.
			name: "maxCallsPerSession=0 is reported at the call site, not here",
			eff: MergePolicies(nil, []ToolPolicy{toolPolicyNamed("tp", ToolPolicySpec{
				Rules: []ToolRule{{Tool: toolRefund, Action: ToolActionAllow, MaxCallsPerSession: i32(0)}},
			})}),
			refs:     AgentReferences{Tools: []string{toolRefund}},
			wantNone: true,
		},
		{
			name: "several violations are all reported",
			eff: MergePolicies([]Policy{policyNamed("p", PolicySpec{
				Models: &AccessRule{Deny: []string{modelGPT4}},
				Tools:  &AccessRule{Deny: []string{toolRefund}},
			})}, nil),
			refs:     AgentReferences{Model: modelGPT4, Tools: []string{toolRefund}},
			wantSubs: []string{`model "gpt-4"`, wantToolRefundQuoted},
		},
		{
			name:     "empty refs never violate",
			eff:      MergePolicies([]Policy{policyNamed("p", PolicySpec{Models: &AccessRule{Allow: []string{modelClaude}}})}, nil),
			refs:     AgentReferences{},
			wantNone: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.eff.Violations(tc.refs)
			if tc.wantNone {
				if len(got) != 0 {
					t.Fatalf("Violations = %v, want none", got)
				}
				return
			}
			joined := strings.Join(got, "\n")
			for _, want := range tc.wantSubs {
				if !strings.Contains(joined, want) {
					t.Errorf("Violations missing %q:\n%s", want, joined)
				}
			}
		})
	}
}

// The coarse Policy.tools rule and a ToolPolicy rule are independent gates; a
// permissive rule in one must not rescue a denial in the other.
func TestViolationsCoarseDenyBeatsToolPolicyAllow(t *testing.T) {
	eff := MergePolicies(
		[]Policy{policyNamed("p", PolicySpec{Tools: &AccessRule{Deny: []string{toolRefund}}})},
		[]ToolPolicy{toolPolicyNamed("tp", ToolPolicySpec{
			Rules: []ToolRule{{Tool: toolRefund, Action: ToolActionAllow}},
		})},
	)
	got := eff.Violations(AgentReferences{Tools: []string{toolRefund}})
	if len(got) != 1 || !strings.Contains(got[0], "denied by policy") {
		t.Errorf("Violations = %v, want the coarse deny to win", got)
	}
}
