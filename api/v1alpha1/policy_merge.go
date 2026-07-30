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

// This file is the single implementation of what a set of Policy and ToolPolicy
// CRs *means*. Both consumers use it, so the two halves of enforcement cannot
// disagree:
//
//   - the Operator merges an Agent's policies and refuses to run the Agent when
//     its declared references are denied (fail fast, visible in status), and
//   - the Registry merges the same way and ships the result to runtimes, which
//     enforce the call-time half (which tool this turn, how many times).

// EffectivePolicy is the merged result of every Policy and ToolPolicy an Agent
// references. Merging is intentionally one-directional: allow lists intersect
// and deny lists union, so attaching another Policy can only narrow what an
// Agent may do — never widen it.
type EffectivePolicy struct {
	// Sources names the Policy and ToolPolicy CRs this was merged from, in
	// resolution order, so a denial can point at the object to edit.
	Sources []string

	Models    *AccessRule
	Memory    *AccessRule
	MCP       *AccessRule
	Tools     *AccessRule
	Workflows *AccessRule

	// ToolRules are the concatenated rules of every referenced ToolPolicy, in
	// order. The first rule matching a tool name applies, except that an exact
	// name match anywhere beats an earlier "*" wildcard.
	ToolRules []ToolRule
	// DefaultToolAction applies when no ToolRule matches. Deny wins: if any
	// referenced ToolPolicy defaults to deny, so does the merged policy.
	DefaultToolAction ToolAction
}

// MergePolicies combines the given Policies and ToolPolicies into one effective
// policy. It returns nil when there is nothing to enforce, so callers can treat
// "no policy referenced" and "policies that constrain nothing" identically.
func MergePolicies(policies []Policy, toolPolicies []ToolPolicy) *EffectivePolicy {
	if len(policies) == 0 && len(toolPolicies) == 0 {
		return nil
	}
	eff := &EffectivePolicy{DefaultToolAction: ToolActionAllow}
	for i := range policies {
		p := &policies[i]
		eff.Sources = append(eff.Sources, "Policy/"+p.Name)
		eff.Models = mergeAccessRule(eff.Models, p.Spec.Models)
		eff.Memory = mergeAccessRule(eff.Memory, p.Spec.Memory)
		eff.MCP = mergeAccessRule(eff.MCP, p.Spec.MCP)
		eff.Tools = mergeAccessRule(eff.Tools, p.Spec.Tools)
		eff.Workflows = mergeAccessRule(eff.Workflows, p.Spec.Workflows)
	}
	for i := range toolPolicies {
		tp := &toolPolicies[i]
		eff.Sources = append(eff.Sources, "ToolPolicy/"+tp.Name)
		eff.ToolRules = append(eff.ToolRules, tp.Spec.Rules...)
		// An unset defaultAction means the CRD default (allow); only an explicit
		// deny tightens the merged default.
		if tp.Spec.DefaultAction == ToolActionDeny {
			eff.DefaultToolAction = ToolActionDeny
		}
	}
	return eff
}

// mergeAccessRule intersects allow lists and unions deny lists. An empty allow
// list means "unconstrained", so intersecting with it yields the other side
// rather than the empty set — otherwise a Policy that only denies would
// accidentally forbid everything.
func mergeAccessRule(into, add *AccessRule) *AccessRule {
	if add == nil {
		return into
	}
	if into == nil {
		out := *add
		return &out
	}
	out := &AccessRule{
		Allow: intersectOrKeep(into.Allow, add.Allow),
		Deny:  union(into.Deny, add.Deny),
	}
	return out
}

// intersectOrKeep intersects two allow lists, treating an empty list as "no
// constraint".
func intersectOrKeep(a, b []string) []string {
	switch {
	case len(a) == 0:
		return b
	case len(b) == 0:
		return a
	}
	inB := make(map[string]bool, len(b))
	for _, v := range b {
		inB[v] = true
	}
	// A wildcard on either side does not narrow the other.
	if inB["*"] {
		return a
	}
	out := make([]string, 0, len(a))
	for _, v := range a {
		if v == "*" || inB[v] {
			out = append(out, v)
		}
	}
	return out
}

func union(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, v := range append(append([]string{}, a...), b...) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// AgentReferences are the names an Agent declares, gathered by the caller (the
// Operator resolves ToolSets and each Tool's MCPServer before checking, so a
// tool reached indirectly is policed the same as a directly referenced one).
type AgentReferences struct {
	Model      string
	Workflow   string
	Memories   []string
	Tools      []string
	MCPServers []string
}

// Violations reports every way the given references breach this policy, as
// human-readable strings suitable for a status condition message. An empty
// result means the Agent's declaration is permitted — call-time behavior is
// still the runtime's to police.
func (e *EffectivePolicy) Violations(refs AgentReferences) []string {
	if e == nil {
		return nil
	}
	var out []string
	if e.Models != nil && refs.Model != "" {
		if !e.Models.Permits(refs.Model) {
			out = append(out, fmt.Sprintf("model %q is denied by policy", refs.Model))
		}
	}
	if e.Workflows != nil && refs.Workflow != "" {
		if !e.Workflows.Permits(refs.Workflow) {
			out = append(out, fmt.Sprintf("workflow %q is denied by policy", refs.Workflow))
		}
	}
	for _, name := range refs.Memories {
		if e.Memory != nil && !e.Memory.Permits(name) {
			out = append(out, fmt.Sprintf("memory %q is denied by policy", name))
		}
	}
	for _, name := range refs.MCPServers {
		if e.MCP != nil && !e.MCP.Permits(name) {
			out = append(out, fmt.Sprintf("mcpServer %q is denied by policy", name))
		}
	}
	for _, name := range refs.Tools {
		if e.Tools != nil && !e.Tools.Permits(name) {
			out = append(out, fmt.Sprintf("tool %q is denied by policy", name))
			continue
		}
		// A tool the Agent declares but may never call is a declaration error
		// worth failing on. A *capped* tool is not: the cap is a call-time
		// concern the runtime enforces.
		if rule := MatchToolRule(e.ToolRules, name); rule != nil {
			if rule.Action == ToolActionDeny {
				out = append(out, fmt.Sprintf("tool %q is denied by a ToolPolicy rule (matched %q)", name, rule.Tool))
			}
		} else if e.DefaultToolAction == ToolActionDeny {
			out = append(out, fmt.Sprintf("tool %q matches no ToolPolicy rule and the default action is deny", name))
		}
	}
	sort.Strings(out)
	return out
}

// Permits reports whether name is allowed by this rule. Deny always wins; a
// non-empty allow list is exhaustive; "*" matches anything.
func (r *AccessRule) Permits(name string) bool {
	if r == nil {
		return true
	}
	for _, d := range r.Deny {
		if d == name || d == "*" {
			return false
		}
	}
	if len(r.Allow) == 0 {
		return true
	}
	for _, a := range r.Allow {
		if a == name || a == "*" {
			return true
		}
	}
	return false
}

// MatchToolRule returns the first rule matching name, or nil when none does. An
// exact name match anywhere in the list beats an earlier "*" wildcard, so a
// catch-all deny may be listed alongside specific allows in any order.
func MatchToolRule(rules []ToolRule, name string) *ToolRule {
	var wildcard *ToolRule
	for i := range rules {
		switch rules[i].Tool {
		case name:
			return &rules[i]
		case "*":
			if wildcard == nil {
				wildcard = &rules[i]
			}
		}
	}
	return wildcard
}
